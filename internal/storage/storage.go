// GoToSocial
// Copyright (C) GoToSocial Authors admin@gotosocial.org
// SPDX-License-Identifier: AGPL-3.0-or-later
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package storage

import (
	"context"
	"fmt"
	"io"
	"mime"
	"net/url"
	"path"
	"time"

	"codeberg.org/gruf/go-bytesize"
	"codeberg.org/gruf/go-cache/v3/ttl"
	"codeberg.org/gruf/go-store/v2/storage"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/superseriousbusiness/gotosocial/internal/config"
	"github.com/superseriousbusiness/gotosocial/internal/gtserror"
	"github.com/superseriousbusiness/gotosocial/internal/log"
)

const (
	urlCacheTTL             = time.Hour * 24
	urlCacheExpiryFrequency = time.Minute * 5
)

// PresignedURL represents a pre signed S3 URL with
// an expiry time.
type PresignedURL struct {
	*url.URL
	Expiry time.Time // link expires at this time
}

var (
	// Ptrs to underlying storage library errors.
	ErrAlreadyExists = storage.ErrAlreadyExists
	ErrNotFound      = storage.ErrNotFound
)

// Driver wraps a kv.KVStore to also provide S3 presigned GET URLs.
type Driver struct {
	// Underlying storage
	Storage storage.Storage

	// S3-only parameters
	Proxy          bool
	Bucket         string
	PresignedCache *ttl.Cache[string, PresignedURL]
}

// Get returns the byte value for key in storage.
func (d *Driver) Get(ctx context.Context, key string) ([]byte, error) {
	return d.Storage.ReadBytes(ctx, key)
}

// GetStream returns an io.ReadCloser for the value bytes at key in the storage.
func (d *Driver) GetStream(ctx context.Context, key string) (io.ReadCloser, error) {
	return d.Storage.ReadStream(ctx, key)
}

// Put writes the supplied value bytes at key in the storage
func (d *Driver) Put(ctx context.Context, key string, value []byte) (int, error) {
	return d.Storage.WriteBytes(ctx, key, value)
}

// PutStream writes the bytes from supplied reader at key in the storage
func (d *Driver) PutStream(ctx context.Context, key string, r io.Reader) (int64, error) {
	return d.Storage.WriteStream(ctx, key, r)
}

// Remove attempts to remove the supplied key (and corresponding value) from storage.
func (d *Driver) Delete(ctx context.Context, key string) error {
	return d.Storage.Remove(ctx, key)
}

// Has checks if the supplied key is in the storage.
func (d *Driver) Has(ctx context.Context, key string) (bool, error) {
	return d.Storage.Stat(ctx, key)
}

// WalkKeys walks the keys in the storage.
func (d *Driver) WalkKeys(ctx context.Context, walk func(context.Context, string) error) error {
	return d.Storage.WalkKeys(ctx, storage.WalkKeysOptions{
		WalkFn: func(ctx context.Context, entry storage.Entry) error {
			if entry.Key == "store.lock" {
				return nil // skip this.
			}
			return walk(ctx, entry.Key)
		},
	})
}

// Close will close the storage, releasing any file locks.
func (d *Driver) Close() error {
	return d.Storage.Close()
}

// URL will return a presigned GET object URL, but only if running on S3 storage with proxying disabled.
func (d *Driver) URL(ctx context.Context, key string) *PresignedURL {
	// Check whether S3 *without* proxying is enabled
	s3, ok := d.Storage.(*storage.S3Storage)
	if !ok || d.Proxy {
		return nil
	}

	// Check cache underlying cache map directly to
	// avoid extending the TTL (which cache.Get() does).
	d.PresignedCache.Lock()
	e, ok := d.PresignedCache.Cache.Get(key)
	d.PresignedCache.Unlock()

	if ok {
		return &e.Value
	}

	u, err := s3.Client().PresignedGetObject(ctx, d.Bucket, key, urlCacheTTL, url.Values{
		"response-content-type": []string{mime.TypeByExtension(path.Ext(key))},
	})
	if err != nil {
		// If URL request fails, fallback is to fetch the file. So ignore the error here
		return nil
	}

	psu := PresignedURL{
		URL:    u,
		Expiry: time.Now().Add(urlCacheTTL), // link expires in 24h time
	}

	d.PresignedCache.Set(key, psu)
	return &psu
}

// ProbeCSPUri returns a URI string that can be added
// to a content-security-policy to allow requests to
// endpoints served by this driver.
//
// If the driver is not backed by non-proxying S3,
// this will return an empty string and no error.
//
// Otherwise, this function probes for a CSP URI by
// doing the following:
//
//  1. Create a temporary file in the S3 bucket.
//  2. Generate a pre-signed URL for that file.
//  3. Extract '[scheme]://[host]' from the URL.
//  4. Remove the temporary file.
//  5. Return the '[scheme]://[host]' string.
func (d *Driver) ProbeCSPUri(ctx context.Context) (string, error) {
	// Check whether S3 without proxying
	// is enabled. If it's not, there's
	// no need to add anything to the CSP.
	s3, ok := d.Storage.(*storage.S3Storage)
	if !ok || d.Proxy {
		return "", nil
	}

	const cspKey = "gotosocial-csp-probe"

	// Create an empty file in S3 storage.
	if _, err := d.Put(ctx, cspKey, make([]byte, 0)); err != nil {
		return "", gtserror.Newf("error putting file in bucket at key %s: %w", cspKey, err)
	}

	// Try to clean up file whatever happens.
	defer func() {
		if err := d.Delete(ctx, cspKey); err != nil {
			log.Warnf(ctx, "error deleting file from bucket at key %s (%v); "+
				"you may want to remove this file manually from your S3 bucket", cspKey, err)
		}
	}()

	// Get a presigned URL for that empty file.
	u, err := s3.Client().PresignedGetObject(ctx, d.Bucket, cspKey, 1*time.Second, nil)
	if err != nil {
		return "", err
	}

	// Create a stripped version of the presigned
	// URL that includes only the host and scheme.
	uStripped := &url.URL{
		Scheme: u.Scheme,
		Host:   u.Host,
	}

	return uStripped.String(), nil
}

func AutoConfig() (*Driver, error) {
	switch backend := config.GetStorageBackend(); backend {
	case "s3":
		return NewS3Storage()
	case "local":
		return NewFileStorage()
	default:
		return nil, fmt.Errorf("invalid storage backend: %s", backend)
	}
}

func NewFileStorage() (*Driver, error) {
	// Load runtime configuration
	basePath := config.GetStorageLocalBasePath()

	// Open the disk storage implementation
	disk, err := storage.OpenDisk(basePath, &storage.DiskConfig{
		// Put the store lockfile in the storage dir itself.
		// Normally this would not be safe, since we could end up
		// overwriting the lockfile if we store a file called 'store.lock'.
		// However, in this case it's OK because the keys are set by
		// GtS and not the user, so we know we're never going to overwrite it.
		LockFile:     path.Join(basePath, "store.lock"),
		WriteBufSize: int(16 * bytesize.KiB),
	})
	if err != nil {
		return nil, fmt.Errorf("error opening disk storage: %w", err)
	}

	return &Driver{
		Storage: disk,
	}, nil
}

func NewS3Storage() (*Driver, error) {
	// Load runtime configuration
	endpoint := config.GetStorageS3Endpoint()
	access := config.GetStorageS3AccessKey()
	secret := config.GetStorageS3SecretKey()
	secure := config.GetStorageS3UseSSL()
	bucket := config.GetStorageS3BucketName()

	// Open the s3 storage implementation
	s3, err := storage.OpenS3(endpoint, bucket, &storage.S3Config{
		CoreOpts: minio.Options{
			Creds:  credentials.NewStaticV4(access, secret, ""),
			Secure: secure,
		},
		GetOpts:      minio.GetObjectOptions{},
		PutOpts:      minio.PutObjectOptions{},
		PutChunkSize: 5 * 1024 * 1024, // 5MiB
		StatOpts:     minio.StatObjectOptions{},
		RemoveOpts:   minio.RemoveObjectOptions{},
		ListSize:     200,
	})
	if err != nil {
		return nil, fmt.Errorf("error opening s3 storage: %w", err)
	}

	// ttl should be lower than the expiry used by S3 to avoid serving invalid URLs
	presignedCache := ttl.New[string, PresignedURL](0, 1000, urlCacheTTL-urlCacheExpiryFrequency)
	presignedCache.Start(urlCacheExpiryFrequency)

	return &Driver{
		Proxy:          config.GetStorageS3Proxy(),
		Bucket:         config.GetStorageS3BucketName(),
		Storage:        s3,
		PresignedCache: presignedCache,
	}, nil
}
