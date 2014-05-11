package upload

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/meatballhat/artifacts/path"
	"github.com/mitchellh/goamz/aws"
	"github.com/mitchellh/goamz/s3"
)

type uploader struct {
	BucketName    string
	Paths         *path.PathSet
	TargetPaths   []string
	CacheControl  string
	Retries       int
	RetryInterval time.Duration
	Concurrency   int
	AccessKey     string
	SecretKey     string
}

// Upload does the deed!
func Upload(opts *Options) {
	newUploader(opts).Upload()
}

func newUploader(opts *Options) *uploader {
	u := &uploader{
		BucketName:    opts.BucketName,
		TargetPaths:   opts.TargetPaths,
		Paths:         path.NewPathSet(),
		Concurrency:   opts.Concurrency,
		Retries:       opts.Retries,
		RetryInterval: 3 * time.Second,
	}

	u.CacheControl = opts.CacheControl

	if opts.Private {
		u.CacheControl = "private"
	}

	if u.CacheControl == "" {
		u.CacheControl = "public, max-age=315360000"
	}

	for _, s := range opts.Paths {
		parts := strings.SplitN(s, ":", 2)
		if len(parts) < 2 {
			parts = append(parts, "")
		}
		u.Paths.Add(path.NewPath(opts.WorkingDir, parts[0], parts[1]))
	}

	return u
}

func (u *uploader) Upload() error {
	done := make(chan bool)
	allDone := 0
	fileChan := u.files()

	for i := 0; i < u.Concurrency; i++ {
		go func() {
			auth, err := aws.GetAuth(u.AccessKey, u.SecretKey)
			if err != nil {
				fmt.Printf("uploader %v failed to get aws auth: %v\n", i, err)
				done <- true
				return
			}

			conn := s3.New(auth, aws.USEast)
			bucket := conn.Bucket(u.BucketName)

			if bucket == nil {
				fmt.Printf("uploader %v failed to get bucket\n", i)
				done <- true
				return
			}

			for artifact := range fileChan {
				u.uploadFile(bucket, artifact)
			}

			done <- true
		}()
	}

	for {
		select {
		case <-done:
			allDone += 1
			if allDone >= u.Concurrency {
				return nil
			}
		}
	}

	return nil
}

func (u *uploader) files() chan *artifact {
	artifacts := make(chan *artifact)

	go func() {
		for _, path := range u.Paths.All() {
			to, from, root := path.To, path.From, path.Root
			if path.IsDir() {
				root = filepath.Join(root, from)
				if strings.HasSuffix(root, "/") {
					root = root + "/"
				}
			}

			filepath.Walk(path.Fullpath(), func(f string, info os.FileInfo, err error) error {
				if info != nil && info.IsDir() {
					return nil
				}

				relPath := strings.Replace(strings.Replace(f, root, "", -1), root+"/", "", -1)
				destination := relPath
				if len(to) > 0 {
					if path.IsDir() {
						destination = filepath.Join(to, relPath)
					} else {
						destination = to
					}
				}

				for _, targetPath := range u.TargetPaths {
					artifacts <- newArtifact(root, relPath, targetPath, destination)
				}
				return nil
			})

		}
		close(artifacts)
	}()

	return artifacts
}

func (u *uploader) uploadFile(b *s3.Bucket, a *artifact) error {
	retries := 0

	for {
		err := u.rawUpload(b, a)
		if err != nil {
			if retries < u.Retries {
				retries += 1
				time.Sleep(u.RetryInterval)
				continue
			} else {
				return err
			}
		}
		return nil
	}
}

func (u *uploader) rawUpload(b *s3.Bucket, a *artifact) error {
	destination := a.FullDestination()
	reader, err := a.Reader()
	if err != nil {
		return err
	}

	fmt.Printf("uploading %q -> %q\n", a.Source, destination)

	err = b.PutReaderHeader(destination, reader, a.Size(),
		map[string][]string{
			"Content-Type":  []string{a.ContentType()},
			"Cache-Control": []string{u.CacheControl},
		}, s3.Private)
	if err != nil {
		fmt.Printf("ERROR: %v\n", err)
		return err
	}

	return nil
}
