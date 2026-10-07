// Licensed under Overwatch License Revision 3 with an upstream rclone PR exception.
// See MYBOX-LICENSE at the repository root. Pre-existing third-party rights are preserved.

// Package mybox provides access to NAVER MYBOX using its public Open API.
package mybox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/rclone/rclone/backend/mybox/api"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/configstruct"
	"github.com/rclone/rclone/fs/fserrors"
	"github.com/rclone/rclone/fs/fshttp"
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/lib/dircache"
	"github.com/rclone/rclone/lib/encoder"
	"github.com/rclone/rclone/lib/pacer"
	"github.com/rclone/rclone/lib/rest"
)

const apiURL = "https://open-api.mybox.naver.com/v1/drive"

// MYBOX rejects filenames beginning with a period, including macOS sidecar files.
const defaultEncoding = encoder.Display | encoder.EncodeInvalidUtf8 | encoder.EncodeLeftPeriod

// rootID is a cache sentinel; it is never sent to the API.
const rootID = "root"

func init() {
	fs.Register(&fs.RegInfo{
		Name: "mybox", Description: "NAVER MYBOX", NewFs: NewFs,
		Options: []fs.Option{{
			Name: "access_token", Required: true, Sensitive: true,
			Help: "Personal access token from MYBOX web settings.\nCreate one under Account and personal access token management.\nReplace the token when it expires.",
		}, {
			Name: config.ConfigEncoding, Help: config.ConfigEncodingHelp, Advanced: true,
			Default: defaultEncoding,
		}},
	})
}

// Options defines the MYBOX configuration.
type Options struct {
	AccessToken string               `config:"access_token"`
	Enc         encoder.MultiEncoder `config:"encoding"`
}

// Fs represents a MYBOX filesystem.
type Fs struct {
	name, root    string
	opt           Options
	features      *fs.Features
	srv, transfer *rest.Client
	dirCache      *dircache.DirCache
	pacer         *fs.Pacer
}

// Object represents a MYBOX file.
type Object struct {
	fs     *Fs
	remote string
	item   api.Resource
}

// NewFs constructs a filesystem from a remote name and root path.
func NewFs(ctx context.Context, name, root string, m configmap.Mapper) (fs.Fs, error) {
	opt := Options{}
	if err := configstruct.Set(m, &opt); err != nil {
		return nil, err
	}
	if opt.AccessToken == "" {
		return nil, errors.New("mybox: access_token is required")
	}
	f := newFs(ctx, name, root, opt, fshttp.NewClient(ctx), apiURL)
	return f.resolveRoot(ctx)
}

func (f *Fs) resolveRoot(ctx context.Context) (fs.Fs, error) {
	err := f.dirCache.FindRoot(ctx, false)
	if err == nil {
		return f, nil
	}
	if !errors.Is(err, fs.ErrorDirNotFound) {
		return nil, err
	}
	// A remote ending in a file must return its parent filesystem.
	parent, leaf := dircache.SplitPath(f.root)
	originalRoot := f.root
	f.root = parent
	f.dirCache = dircache.New(parent, rootID, f)
	_, err = f.NewObject(ctx, leaf)
	if err == nil {
		return f, fs.ErrorIsFile
	}
	if !errors.Is(err, fs.ErrorObjectNotFound) && !errors.Is(err, fs.ErrorDirNotFound) {
		return nil, err
	}
	f.root = originalRoot
	f.dirCache = dircache.New(f.root, rootID, f)
	return f, nil
}

func newFs(ctx context.Context, name, root string, opt Options, client *http.Client, endpoint string) *Fs {
	f := &Fs{name: name, root: strings.Trim(root, "/"), opt: opt,
		srv: rest.NewClient(client).SetRoot(endpoint).SetHeader("Authorization", "Bearer "+opt.AccessToken),
		// Signed transfer URLs authenticate themselves; never send the PAT to storage hosts.
		transfer: rest.NewClient(client),
		pacer:    fs.NewPacer(ctx, pacer.NewDefault(pacer.MinSleep(time.Second), pacer.MaxSleep(60*time.Second), pacer.DecayConstant(2))),
	}
	f.dirCache = dircache.New(f.root, rootID, f)
	f.features = (&fs.Features{CanHaveEmptyDirectories: true}).Fill(ctx, f)
	return f
}

// Name returns the configured remote name.
func (f *Fs) Name() string { return f.name }

// Root returns the configured root path.
func (f *Fs) Root() string { return f.root }

// String describes the filesystem.
func (f *Fs) String() string { return "NAVER MYBOX root '" + f.root + "'" }

// Precision returns the timestamp precision accepted during upload.
func (f *Fs) Precision() time.Duration { return time.Second }

// Hashes returns the supported hashes. MYBOX does not expose checksums.
func (f *Fs) Hashes() hash.Set { return hash.NewHashSet() }

// Features returns the optional filesystem capabilities.
func (f *Fs) Features() *fs.Features { return f.features }

func (f *Fs) call(ctx context.Context, method, endpoint string, params url.Values, request, response any) (resp *http.Response, err error) {
	err = f.pacer.Call(func() (bool, error) {
		resp, err = f.srv.CallJSON(ctx, &rest.Opts{Method: method, Path: endpoint, Parameters: params, NoResponse: response == nil}, request, response)
		if fserrors.ContextError(ctx, &err) {
			return false, err
		}
		if resp != nil && (resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable) {
			if delay, parseErr := strconv.Atoi(resp.Header.Get("Retry-After")); parseErr == nil && delay >= 0 {
				return true, pacer.RetryAfterError(err, time.Duration(delay)*time.Second)
			}
			if when, parseErr := http.ParseTime(resp.Header.Get("Retry-After")); parseErr == nil && time.Until(when) > 0 {
				return true, pacer.RetryAfterError(err, time.Until(when))
			}
		}
		return fserrors.ShouldRetry(err) || fserrors.ShouldRetryHTTP(resp, []int{429, 500, 502, 503, 504}), err
	})
	return resp, err
}

func parentID(id string) string {
	if id == rootID {
		return ""
	}
	return id
}

// list visits pages until the callback stops or the directory is exhausted.
func (f *Fs) list(ctx context.Context, id string, visit func(api.Resource) bool) error {
	endpoint := "/resources"
	if id != rootID {
		endpoint = "/folders/" + url.PathEscape(id) + "/resources"
	}
	params := url.Values{"count": {"1000"}}
	seen := map[string]bool{}
	for {
		var page api.Listing
		resp, err := f.call(ctx, "GET", endpoint, params, nil, &page)
		if err != nil {
			if resp != nil && resp.StatusCode == http.StatusNotFound {
				return fs.ErrorDirNotFound
			}
			return err
		}
		for _, item := range page.Resources {
			item.Name = f.opt.Enc.ToStandardName(item.Name)
			if visit(item) {
				return nil
			}
		}
		cursor := page.Metadata.NextCursor
		if cursor == "" {
			return nil
		}
		if seen[cursor] {
			return errors.New("mybox: repeated pagination cursor")
		}
		seen[cursor] = true
		params.Set("cursor", cursor)
	}
}

// FindLeaf finds a child directory for the directory cache.
func (f *Fs) FindLeaf(ctx context.Context, pathID, leaf string) (id string, found bool, err error) {
	err = f.list(ctx, pathID, func(item api.Resource) bool {
		if item.Type == "folder" && item.Name == leaf {
			id, found = item.ID, true
		}
		return found
	})
	return
}

// CreateDir creates a child directory for the directory cache.
func (f *Fs) CreateDir(ctx context.Context, pathID, leaf string) (string, error) {
	var item api.Resource
	resp, err := f.call(ctx, "POST", "/folders", nil, &api.CreateFolder{Name: f.opt.Enc.FromStandardName(leaf), ParentID: parentID(pathID)}, &item)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusConflict {
			id, found, lookupErr := f.FindLeaf(ctx, pathID, leaf)
			if lookupErr == nil && found {
				return id, nil
			}
		}
		return "", err
	}
	if item.ID == "" {
		return "", errors.New("mybox: folder creation returned no resourceId")
	}
	return item.ID, nil
}

// List returns the files and directories immediately below dir.
func (f *Fs) List(ctx context.Context, dir string) (entries fs.DirEntries, err error) {
	id, err := f.dirCache.FindDir(ctx, dir, false)
	if err != nil {
		return nil, err
	}
	err = f.list(ctx, id, func(item api.Resource) bool {
		remote := path.Join(dir, item.Name)
		if item.Type == "folder" {
			f.dirCache.Put(remote, item.ID)
			entries = append(entries, fs.NewDir(remote, item.ModifiedAt).SetID(item.ID))
		} else if item.Type == "file" {
			entries = append(entries, &Object{fs: f, remote: remote, item: item})
		}
		return false
	})
	return
}

// NewObject finds a file, returning ErrorObjectNotFound if absent.
func (f *Fs) NewObject(ctx context.Context, remote string) (fs.Object, error) {
	leaf, id, err := f.dirCache.FindPath(ctx, remote, false)
	if errors.Is(err, fs.ErrorDirNotFound) {
		return nil, fs.ErrorObjectNotFound
	}
	if err != nil {
		return nil, err
	}
	var object *Object
	isDir := false
	err = f.list(ctx, id, func(item api.Resource) bool {
		if item.Name != leaf {
			return false
		}
		isDir = item.Type == "folder"
		if item.Type == "file" {
			object = &Object{fs: f, remote: remote, item: item}
		}
		return true
	})
	if errors.Is(err, fs.ErrorDirNotFound) {
		return nil, fs.ErrorObjectNotFound
	}
	if err != nil {
		return nil, err
	}
	if isDir {
		return nil, fs.ErrorIsDir
	}
	if object == nil {
		return nil, fs.ErrorObjectNotFound
	}
	return object, nil
}

// Put uploads a file, replacing any existing file with the same name.
func (f *Fs) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	o := &Object{fs: f, remote: src.Remote()}
	if err := o.Update(ctx, in, src, options...); err != nil {
		return nil, err
	}
	return o, nil
}

// Mkdir creates dir and any missing ancestors.
func (f *Fs) Mkdir(ctx context.Context, dir string) error {
	_, err := f.dirCache.FindDir(ctx, dir, true)
	return err
}

// Rmdir moves an empty directory to the trash.
func (f *Fs) Rmdir(ctx context.Context, dir string) error {
	id, err := f.dirCache.FindDir(ctx, dir, false)
	if err != nil {
		return err
	}
	notEmpty := false
	err = f.list(ctx, id, func(api.Resource) bool { notEmpty = true; return true })
	if err != nil {
		return err
	}
	if notEmpty {
		return fs.ErrorDirectoryNotEmpty
	}
	if id == rootID {
		return nil
	}
	if err = f.remove(ctx, id); err != nil {
		if errors.Is(err, fs.ErrorObjectNotFound) {
			return fs.ErrorDirNotFound
		}
		return err
	}
	f.dirCache.FlushDir(dir)
	return nil
}

func (f *Fs) remove(ctx context.Context, id string) error {
	resp, err := f.call(ctx, "DELETE", "/resources/"+url.PathEscape(id), nil, nil, nil)
	if resp != nil && resp.StatusCode == http.StatusNotFound {
		return fs.ErrorObjectNotFound
	}
	return err
}

// rename changes a resource's name within its current parent directory.
func (f *Fs) rename(ctx context.Context, id, name string) (*http.Response, error) {
	return f.call(ctx, "POST", "/resources/"+url.PathEscape(id)+"/rename", nil,
		&api.RenameRequest{Name: f.opt.Enc.FromStandardName(name)}, nil)
}

// Move renames a file within its parent. Cross-directory moves use rclone's fallback.
func (f *Fs) Move(ctx context.Context, src fs.Object, remote string) (fs.Object, error) {
	srcObj, ok := src.(*Object)
	if !ok || srcObj.fs.opt.AccessToken != f.opt.AccessToken {
		return nil, fs.ErrorCantMove
	}
	srcPath, dstPath := path.Join(srcObj.fs.root, srcObj.remote), path.Join(f.root, remote)
	if path.Dir(srcPath) != path.Dir(dstPath) {
		return nil, fs.ErrorCantMove
	}
	if srcPath != dstPath {
		resp, err := f.rename(ctx, srcObj.item.ID, path.Base(dstPath))
		if err != nil {
			if resp != nil && resp.StatusCode == http.StatusNotFound {
				return nil, fs.ErrorObjectNotFound
			}
			return nil, err
		}
	}
	item := srcObj.item
	item.Name = path.Base(remote)
	return &Object{fs: f, remote: remote, item: item}, nil
}

// DirMove renames a directory within its parent without moving its contents.
// Existing destinations return ErrorDirExists; cross-directory moves return ErrorCantDirMove.
func (f *Fs) DirMove(ctx context.Context, src fs.Fs, srcRemote, dstRemote string) error {
	srcFs, ok := src.(*Fs)
	if !ok || srcFs.opt.AccessToken != f.opt.AccessToken {
		return fs.ErrorCantDirMove
	}
	srcPath, dstPath := path.Join(srcFs.root, srcRemote), path.Join(f.root, dstRemote)
	if path.Dir(srcPath) != path.Dir(dstPath) {
		return fs.ErrorCantDirMove
	}
	id, _, _, _, leaf, err := f.dirCache.DirMove(ctx, srcFs.dirCache, srcFs.root, srcRemote, f.root, dstRemote)
	if err != nil {
		return err
	}
	resp, err := f.rename(ctx, id, leaf)
	if err != nil {
		if resp != nil {
			switch resp.StatusCode {
			case http.StatusConflict:
				return fs.ErrorDirExists
			case http.StatusNotFound:
				return fs.ErrorDirNotFound
			}
		}
		return err
	}
	srcFs.dirCache.FlushDir(srcRemote)
	f.dirCache.FlushDir(dstRemote)
	return nil
}

// About returns account storage usage.
func (f *Fs) About(ctx context.Context) (*fs.Usage, error) {
	var storage api.Storage
	_, err := f.call(ctx, "GET", "/storage", nil, nil, &storage)
	if err != nil {
		return nil, err
	}
	return &fs.Usage{Total: fs.NewUsageValue(storage.Total), Used: fs.NewUsageValue(storage.Used), Free: fs.NewUsageValue(max(0, storage.Total-storage.Used))}, nil
}

// DirCacheFlush discards cached directory IDs.
func (f *Fs) DirCacheFlush() { f.dirCache.ResetRoot() }

// Fs returns the filesystem containing the object.
func (o *Object) Fs() fs.Info { return o.fs }

// Remote returns the path relative to the filesystem root.
func (o *Object) Remote() string { return o.remote }

// String returns the object's path.
func (o *Object) String() string {
	if o == nil {
		return "<nil>"
	}
	return o.remote
}

// Size returns the file size in bytes.
func (o *Object) Size() int64 { return o.item.Size }

// ModTime returns the server's modification timestamp.
func (o *Object) ModTime(context.Context) time.Time { return o.item.ModifiedAt }

// Storable reports that the object is a regular file.
func (o *Object) Storable() bool { return true }

// ID returns the resource ID.
func (o *Object) ID() string { return o.item.ID }

// Hash returns ErrUnsupported because the API exposes no checksums.
func (o *Object) Hash(context.Context, hash.Type) (string, error) { return "", hash.ErrUnsupported }

// SetModTime requires re-uploading the file to change its timestamp.
func (o *Object) SetModTime(context.Context, time.Time) error { return fs.ErrorCantSetModTime }

// Remove moves the file to the trash.
func (o *Object) Remove(ctx context.Context) error { return o.fs.remove(ctx, o.item.ID) }

// Open obtains a fresh download URL and opens the file, supporting byte ranges.
func (o *Object) Open(ctx context.Context, options ...fs.OpenOption) (io.ReadCloser, error) {
	var download api.Download
	resp, err := o.fs.call(ctx, "GET", "/files/"+url.PathEscape(o.item.ID)+"/download", nil, nil, &download)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return nil, fs.ErrorObjectNotFound
		}
		return nil, err
	}
	if download.URL == "" {
		return nil, errors.New("mybox: missing download URL")
	}
	fs.FixRangeOption(options, o.Size())
	resp, err = o.fs.transfer.Call(ctx, &rest.Opts{Method: "GET", RootURL: download.URL, Options: options})
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// Update uploads the file using a single-use multipart URL.
func (o *Object) Update(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) error {
	size := src.Size()
	if size < 0 {
		return errors.New("mybox: upload requires a known file size")
	}
	leaf, id, err := o.fs.dirCache.FindPath(ctx, o.remote, true)
	if err != nil {
		return err
	}
	var upload api.Upload
	_, err = o.fs.call(ctx, "POST", "/files", nil, &api.UploadRequest{
		Name: o.fs.opt.Enc.FromStandardName(leaf), Size: size, ParentID: parentID(id),
		Overwrite: true, ModifiedTime: src.ModTime(ctx).Format(time.RFC3339), Resume: false,
	}, &upload)
	if err != nil {
		return err
	}
	if upload.URL == "" || upload.Offset != 0 {
		return errors.New("mybox: invalid upload URL or unexpected resume offset")
	}
	// Streaming bodies and single-use URLs must not be retried at this level.
	_, err = o.fs.transfer.CallJSON(ctx, &rest.Opts{
		Method: "POST", RootURL: upload.URL, Body: in, ContentLength: &size,
		MultipartContentName: "Filedata", MultipartFileName: o.fs.opt.Enc.FromStandardName(leaf),
		Options: options, NoResponse: true,
	}, nil, nil)
	if err != nil {
		return err
	}
	object, err := o.fs.NewObject(ctx, o.remote)
	if err != nil {
		return fmt.Errorf("mybox: reading uploaded file metadata: %w", err)
	}
	o.item = object.(*Object).item
	return nil
}

var (
	_ fs.Fs              = (*Fs)(nil)
	_ fs.Object          = (*Object)(nil)
	_ fs.Abouter         = (*Fs)(nil)
	_ fs.Mover           = (*Fs)(nil)
	_ fs.DirMover        = (*Fs)(nil)
	_ fs.DirCacheFlusher = (*Fs)(nil)
	_ fs.IDer            = (*Object)(nil)
	_ dircache.DirCacher = (*Fs)(nil)
)
