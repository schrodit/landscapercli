// SPDX-FileCopyrightText: 2020 SAP SE or an SAP affiliate company and Gardener contributors.
//
// SPDX-License-Identifier: Apache-2.0

package componentsregistry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"

	cdv2 "github.com/gardener/component-spec/bindings-go/apis/v2"
	"github.com/gardener/component-spec/bindings-go/codec"
	"github.com/gardener/component-spec/bindings-go/ctf"
	"github.com/go-logr/logr"
	"github.com/mandelsoft/vfs/pkg/osfs"
	"github.com/mandelsoft/vfs/pkg/projectionfs"
	"github.com/mandelsoft/vfs/pkg/vfs"
	"github.com/opencontainers/go-digest"

	"github.com/gardener/landscaper/pkg/utils"
)

// LocalRepositoryType defines the local repository context type.
const LocalRepositoryType = "local"

// FilesystemBlobType is the access type of a blob that is located in a filesystem.
const FilesystemBlobType = "filesystemBlob"

// NewFilesystemBlobAccess creates a new localFilesystemBlob accessor.
func NewFilesystemBlobAccess(path string) cdv2.TypedObjectAccessor {
	return &FilesystemBlobAccess{
		ObjectType: cdv2.ObjectType{
			Type: FilesystemBlobType,
		},
		Path: path,
	}
}

// FilesystemBlobAccess describes the access for a blob on the filesystem.
type FilesystemBlobAccess struct {
	cdv2.ObjectType `json:",inline"`
	// Path to the file on the filesystem
	Path string `json:"filename"`
}

func (a FilesystemBlobAccess) GetData() ([]byte, error) {
	return json.Marshal(a)
}

func (a *FilesystemBlobAccess) SetData(bytes []byte) error {
	var newAccess FilesystemBlobAccess
	if err := json.Unmarshal(bytes, &newAccess); err != nil {
		return err
	}
	a.Path = newAccess.Path
	return nil
}

// localClient is a component descriptor repository implementation
// that resolves component references stored locally.
// A ComponentDescriptor is resolved by traversing the given paths and decode every found file as component descriptor.
// todo: build cache to not read every file with every resolve attempt.
type localClient struct {
	log logr.Logger
	fs  vfs.FileSystem
}

// NewLocalClient creates a new local registry from a root.
func NewLocalClient(log logr.Logger, rootPath string) (TypedRegistry, error) {
	fs, err := projectionfs.New(osfs.New(), rootPath)
	if err != nil {
		return nil, err
	}
	return &localClient{
		log: log,
		fs:  fs,
	}, nil
}

// Type return the oci registry type that can be handled by this ociClient
func (c *localClient) Type() string {
	return LocalRepositoryType
}

// Get resolves a reference and returns the component descriptor.
func (c *localClient) Resolve(_ context.Context, repoCtx cdv2.RepositoryContext, name, version string) (*cdv2.ComponentDescriptor, ctf.BlobResolver, error) {
	if repoCtx.Type != LocalRepositoryType {
		return nil, nil, fmt.Errorf("unsupported type %s expected %s", repoCtx.Type, LocalRepositoryType)
	}

	cd, localFilesystemBlobResolver, err := c.searchInFs(name, version)
	if err != nil {
		return nil, nil, err
	}
	fsBlobResolver := &FilesystemBlobResolver{
		BaseFilesystemBlobResolver: BaseFilesystemBlobResolver{fs: c.fs},
	}
	aggBlobResolver, err := ctf.NewAggregatedBlobResolver(localFilesystemBlobResolver, fsBlobResolver)
	return cd, aggBlobResolver, err
}

func (c *localClient) searchInFs(name, version string) (*cdv2.ComponentDescriptor, ctf.BlobResolver, error) {
	foundErr := errors.New("comp found")
	var cd *cdv2.ComponentDescriptor
	var resolver ctf.BlobResolver
	err := vfs.Walk(c.fs, "/", func(path string, info os.FileInfo, err error) error {
		// ignore errors
		if err != nil {
			c.log.V(3).Info(err.Error())
			return nil
		}

		if info.IsDir() {
			return nil
		}

		if info.Name() != ctf.ComponentDescriptorFileName {
			return nil
		}

		data, err := vfs.ReadFile(c.fs, path)
		if err != nil {
			return err
		}

		tmpCD := &cdv2.ComponentDescriptor{}
		if err := codec.Decode(data, tmpCD); err != nil {
			return fmt.Errorf("unable to decode component descriptor file %s: %w", path, err)
		}

		if tmpCD.GetName() == name && tmpCD.GetVersion() == version {
			cd = tmpCD

			fs, err := projectionfs.New(c.fs, filepath.Dir(path))
			if err != nil {
				return err
			}
			resolver = &LocalFilesystemBlobResolver{
				BaseFilesystemBlobResolver: BaseFilesystemBlobResolver{fs: fs},
			}
			return foundErr
		}
		return nil
	})
	if err == nil {
		return nil, nil, cdv2.NotFound
	}
	if err != foundErr {
		return nil, nil, err
	}
	if cd == nil {
		return nil, nil, cdv2.NotFound
	}
	return cd, resolver, nil
}

// FilesystemBlobResolver implements the BlobResolver interface for
// "filesystemBlob" access types.
type FilesystemBlobResolver struct {
	BaseFilesystemBlobResolver
}

func (ca *FilesystemBlobResolver) CanResolve(resource cdv2.Resource) bool {
	return resource.Access != nil && resource.Access.GetType() == FilesystemBlobType
}

func (ca *FilesystemBlobResolver) Info(ctx context.Context, res cdv2.Resource) (*ctf.BlobInfo, error) {
	info, file, err := ca.resolve(ctx, res)
	if err != nil {
		return nil, err
	}
	if file != nil {
		if err := file.Close(); err != nil {
			return nil, err
		}
	}
	return info, nil
}

// Resolve fetches the blob for a given resource and writes it to the given tar.
func (ca *FilesystemBlobResolver) Resolve(ctx context.Context, res cdv2.Resource, writer io.Writer) (*ctf.BlobInfo, error) {
	info, file, err := ca.resolve(ctx, res)
	if err != nil {
		return nil, err
	}
	if _, err := io.Copy(writer, file); err != nil {
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	return info, nil
}

func (ca *FilesystemBlobResolver) resolve(_ context.Context, res cdv2.Resource) (*ctf.BlobInfo, io.ReadCloser, error) {
	if res.Access == nil || res.Access.GetType() != FilesystemBlobType {
		return nil, nil, ctf.UnsupportedResolveType
	}
	fsAccess := &FilesystemBlobAccess{}
	if err := cdv2.NewCodec(nil, nil, nil).Decode(res.Access.Raw, fsAccess); err != nil {
		return nil, nil, fmt.Errorf("unable to decode access to type '%s': %w", res.Access.GetType(), err)
	}

	info, file, err := ca.ResolveFromFs(fsAccess.Path)
	if err != nil {
		return nil, nil, err
	}
	info.MediaType = res.Type
	return info, file, nil
}

// LocalFilesystemBlobResolver implements the BlobResolver interface for
// "localFilesystemBlob" access types.
type LocalFilesystemBlobResolver struct {
	BaseFilesystemBlobResolver
}

func (ca *LocalFilesystemBlobResolver) CanResolve(resource cdv2.Resource) bool {
	return resource.Access != nil && resource.Access.GetType() == cdv2.LocalFilesystemBlobType
}

func (ca *LocalFilesystemBlobResolver) Info(_ context.Context, res cdv2.Resource) (*ctf.BlobInfo, error) {
	info, file, err := ca.resolve(res)
	if err != nil {
		return nil, err
	}
	if file != nil {
		if err := file.Close(); err != nil {
			return nil, err
		}
	}
	return info, nil
}

// Resolve fetches the blob for a given resource and writes it to the given tar.
func (ca *LocalFilesystemBlobResolver) Resolve(_ context.Context, res cdv2.Resource, writer io.Writer) (*ctf.BlobInfo, error) {
	info, file, err := ca.resolve(res)
	if err != nil {
		return nil, err
	}
	if _, err := io.Copy(writer, file); err != nil {
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	return info, nil
}

func (ca *LocalFilesystemBlobResolver) resolve(res cdv2.Resource) (*ctf.BlobInfo, io.ReadCloser, error) {
	if res.Access == nil || res.Access.GetType() != cdv2.LocalFilesystemBlobType {
		return nil, nil, ctf.UnsupportedResolveType
	}
	localFSAccess := &cdv2.LocalFilesystemBlobAccess{}
	if err := cdv2.NewCodec(nil, nil, nil).Decode(res.Access.Raw, localFSAccess); err != nil {
		return nil, nil, fmt.Errorf("unable to decode access to type '%s': %w", res.Access.GetType(), err)
	}
	blobpath := ctf.BlobPath(localFSAccess.Filename)

	info, file, err := ca.ResolveFromFs(blobpath)
	if err != nil {
		return nil, nil, err
	}
	info.MediaType = res.Type
	return info, file, nil
}

// BaseFilesystemBlobResolver implements a common method for filesystem.
type BaseFilesystemBlobResolver struct {
	fs vfs.FileSystem
}

// ResolveFromFs
func (res *BaseFilesystemBlobResolver) ResolveFromFs(blobpath string) (*ctf.BlobInfo, io.ReadCloser, error) {
	info, err := res.fs.Stat(blobpath)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to get fileinfo for %s: %w", blobpath, err)
	}
	if info.IsDir() {
		var data bytes.Buffer
		if err := utils.BuildTarGzip(res.fs, blobpath, &data); err != nil {
			return nil, nil, fmt.Errorf("unable to build tar gz: %w", err)
		}
		return &ctf.BlobInfo{
			MediaType: "",
			Digest:    digest.FromBytes(data.Bytes()).String(),
			Size:      int64(data.Len()),
		}, ioutil.NopCloser(&data), nil
	}
	file, err := res.fs.Open(blobpath)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to open blob from %s", blobpath)
	}

	dig, err := digest.FromReader(file)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to generate dig from %s: %w", blobpath, err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, nil, fmt.Errorf("unable to reset file reader: %w", err)
	}
	return &ctf.BlobInfo{
		MediaType: "",
		Digest:    dig.String(),
		Size:      info.Size(),
	}, file, nil
}
