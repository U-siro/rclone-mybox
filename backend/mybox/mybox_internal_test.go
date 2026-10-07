// Licensed under Overwatch License Revision 3 with an upstream rclone PR exception.
// See MYBOX-LICENSE at the repository root. Pre-existing third-party rights are preserved.

package mybox

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rclone/rclone/backend/mybox/api"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/object"
	"github.com/rclone/rclone/lib/pacer"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testFs(t *testing.T, root string) (*Fs, *http.ServeMux, string) {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/drive/") {
			assert.Equal(t, "Bearer test-pat", r.Header.Get("Authorization"))
		} else {
			assert.Empty(t, r.Header.Get("Authorization"), "PAT must not reach storage hosts")
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	f := newFs(context.Background(), "test", root, Options{AccessToken: "test-pat", Enc: defaultEncoding}, srv.Client(), srv.URL+"/v1/drive")
	f.pacer = fs.NewPacer(context.Background(), pacer.NewDefault(pacer.MinSleep(time.Millisecond), pacer.MaxSleep(2*time.Millisecond)))
	return f, mux, srv.URL
}

func jsonResponse(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	assert.NoError(t, json.NewEncoder(w).Encode(v))
}

func TestListPaginationAndLookup(t *testing.T) {
	f, mux, _ := testFs(t, "")
	stamp := time.Date(2026, 8, 11, 9, 0, 0, 0, time.FixedZone("KST", 9*3600))
	pages := 0
	mux.HandleFunc("GET /v1/drive/resources", func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "1000", r.URL.Query().Get("count"))
		pages++
		var page api.Listing
		if r.URL.Query().Get("cursor") == "" {
			page.Resources = []api.Resource{{ID: "folder", Name: "자료", Type: "folder", ModifiedAt: stamp}}
			page.Metadata.NextCursor = "next+/="
		} else {
			assert.Equal(t, "next+/=", r.URL.Query().Get("cursor"))
			page.Resources = []api.Resource{{ID: "file", Name: "회의록.pdf", Type: "file", Size: 42, ModifiedAt: stamp}}
		}
		jsonResponse(t, w, page)
	})
	entries, err := f.List(context.Background(), "")
	require.NoError(t, err)
	require.Len(t, entries, 2)
	assert.Equal(t, 2, pages)
	assert.Equal(t, "자료", entries[0].Remote())
	assert.Equal(t, "회의록.pdf", entries[1].Remote())
	assert.Equal(t, int64(42), entries[1].Size())
	assert.True(t, stamp.Equal(entries[1].ModTime(context.Background())))
	id, ok := f.dirCache.Get("자료")
	assert.True(t, ok)
	assert.Equal(t, "folder", id)
	o, err := f.NewObject(context.Background(), "회의록.pdf")
	require.NoError(t, err)
	assert.Equal(t, "file", o.(*Object).ID())
	_, err = f.NewObject(context.Background(), "자료")
	assert.ErrorIs(t, err, fs.ErrorIsDir)
	_, err = f.NewObject(context.Background(), "missing")
	assert.ErrorIs(t, err, fs.ErrorObjectNotFound)
}

func TestRepeatedCursor(t *testing.T) {
	f, mux, _ := testFs(t, "")
	mux.HandleFunc("GET /v1/drive/resources", func(w http.ResponseWriter, r *http.Request) {
		var page api.Listing
		page.Metadata.NextCursor = "same"
		jsonResponse(t, w, page)
	})
	_, err := f.List(context.Background(), "")
	assert.ErrorContains(t, err, "repeated pagination cursor")
}

func TestNestedRootAndMkdir(t *testing.T) {
	f, mux, _ := testFs(t, "base")
	mux.HandleFunc("GET /v1/drive/resources", func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(t, w, api.Listing{Resources: []api.Resource{{ID: "base-id", Name: "base", Type: "folder"}}})
	})
	mux.HandleFunc("GET /v1/drive/folders/base-id/resources", func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(t, w, api.Listing{})
	})
	creates := 0
	mux.HandleFunc("POST /v1/drive/folders", func(w http.ResponseWriter, r *http.Request) {
		var request api.CreateFolder
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&request))
		assert.Equal(t, "base-id", request.ParentID)
		assert.Equal(t, "하위", request.Name)
		creates++
		w.WriteHeader(http.StatusCreated)
		jsonResponse(t, w, api.Resource{ID: "child-id", Name: request.Name})
	})
	require.NoError(t, f.Mkdir(context.Background(), "하위"))
	require.NoError(t, f.Mkdir(context.Background(), "하위"))
	assert.Equal(t, 1, creates)
	f.DirCacheFlush()
	_, ok := f.dirCache.Get("하위")
	assert.False(t, ok)
}

func TestUpload(t *testing.T) {
	for _, contents := range []string{"", "hello MYBOX"} {
		t.Run(fmt.Sprintf("size%d", len(contents)), func(t *testing.T) {
			f, mux, serverURL := testFs(t, "")
			stamp := time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC)
			name := "회의록.txt"
			item := api.Resource{ID: "uploaded", Name: name, Type: "file", Size: int64(len(contents)), ModifiedAt: stamp}
			requests := 0
			mux.HandleFunc("POST /v1/drive/files", func(w http.ResponseWriter, r *http.Request) {
				var request map[string]any
				assert.NoError(t, json.NewDecoder(r.Body).Decode(&request))
				assert.Equal(t, name, request["fileName"])
				assert.Equal(t, float64(len(contents)), request["fileSize"])
				assert.Equal(t, true, request["isOverwrite"])
				assert.Equal(t, false, request["resume"])
				assert.Equal(t, stamp.Format(time.RFC3339), request["modifiedTime"])
				assert.NotContains(t, request, "parentId")
				requests++
				jsonResponse(t, w, api.Upload{URL: serverURL + "/upload"})
			})
			mux.HandleFunc("POST /upload", func(w http.ResponseWriter, r *http.Request) {
				assert.Greater(t, r.ContentLength, int64(len(contents)))
				reader, err := r.MultipartReader()
				if !assert.NoError(t, err) {
					return
				}
				part, err := reader.NextPart()
				if !assert.NoError(t, err) {
					return
				}
				assert.Equal(t, "Filedata", part.FormName())
				assert.Equal(t, name, part.FileName())
				got, err := io.ReadAll(part)
				assert.NoError(t, err)
				assert.Equal(t, contents, string(got))
				_, err = reader.NextPart()
				assert.ErrorIs(t, err, io.EOF)
				w.WriteHeader(http.StatusCreated)
			})
			mux.HandleFunc("GET /v1/drive/resources", func(w http.ResponseWriter, r *http.Request) {
				jsonResponse(t, w, api.Listing{Resources: []api.Resource{item}})
			})
			src := object.NewStaticObjectInfo(name, stamp, int64(len(contents)), true, nil, f)
			o, err := f.Put(context.Background(), strings.NewReader(contents), src)
			require.NoError(t, err)
			assert.Equal(t, int64(len(contents)), o.Size())
			assert.Equal(t, "uploaded", o.(*Object).ID())
			require.NoError(t, o.Update(context.Background(), strings.NewReader(contents), src))
			assert.Equal(t, 2, requests)
		})
	}
}

func TestUploadFailures(t *testing.T) {
	for _, mode := range []string{"unknown-size", "offset", "missing-url", "storage-error"} {
		t.Run(mode, func(t *testing.T) {
			f, mux, serverURL := testFs(t, "")
			calls := 0
			mux.HandleFunc("POST /v1/drive/files", func(w http.ResponseWriter, r *http.Request) {
				upload := api.Upload{URL: serverURL + "/upload"}
				if mode == "offset" {
					upload.Offset = 1
				}
				if mode == "missing-url" {
					upload.URL = ""
				}
				jsonResponse(t, w, upload)
			})
			mux.HandleFunc("POST /upload", func(w http.ResponseWriter, r *http.Request) {
				calls++
				http.Error(w, "failed", http.StatusServiceUnavailable)
			})
			size := int64(3)
			if mode == "unknown-size" {
				size = -1
			}
			src := object.NewStaticObjectInfo("file", time.Now(), size, true, nil, f)
			_, err := f.Put(context.Background(), strings.NewReader("abc"), src)
			require.Error(t, err)
			if mode == "storage-error" {
				assert.Equal(t, 1, calls)
			} else {
				assert.Zero(t, calls)
			}
		})
	}
}

func TestOpenRangeAndFreshURL(t *testing.T) {
	f, mux, serverURL := testFs(t, "")
	calls := 0
	mux.HandleFunc("GET /v1/drive/files/file-id/download", func(w http.ResponseWriter, r *http.Request) {
		calls++
		jsonResponse(t, w, api.Download{URL: fmt.Sprintf("%s/download?token=%d", serverURL, calls)})
	})
	mux.HandleFunc("GET /download", func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, fmt.Sprint(calls), r.URL.Query().Get("token"))
		assert.Equal(t, "bytes=1-3", r.Header.Get("Range"))
		w.WriteHeader(http.StatusPartialContent)
		_, err := io.WriteString(w, "bcd")
		assert.NoError(t, err)
	})
	o := &Object{fs: f, item: api.Resource{ID: "file-id", Size: 5}}
	for range 2 {
		body, err := o.Open(context.Background(), &fs.RangeOption{Start: 1, End: 3})
		require.NoError(t, err)
		data, err := io.ReadAll(body)
		require.NoError(t, err)
		require.NoError(t, body.Close())
		assert.Equal(t, "bcd", string(data))
	}
	assert.Equal(t, 2, calls)
}

func TestRmdirAndRemove(t *testing.T) {
	f, mux, _ := testFs(t, "")
	require.NoError(t, f.dirCache.FindRoot(context.Background(), false))
	f.dirCache.Put("folder", "folder-id")
	empty := false
	deletes := 0
	mux.HandleFunc("GET /v1/drive/folders/folder-id/resources", func(w http.ResponseWriter, r *http.Request) {
		page := api.Listing{}
		if !empty {
			page.Resources = []api.Resource{{ID: "file-id", Name: "child", Type: "file"}}
		}
		jsonResponse(t, w, page)
	})
	mux.HandleFunc("DELETE /v1/drive/resources/{id}", func(w http.ResponseWriter, r *http.Request) {
		assert.Contains(t, []string{"file-id", "folder-id"}, r.PathValue("id"))
		deletes++
		w.WriteHeader(http.StatusNoContent)
	})
	assert.ErrorIs(t, f.Rmdir(context.Background(), "folder"), fs.ErrorDirectoryNotEmpty)
	assert.Zero(t, deletes)
	o := &Object{fs: f, item: api.Resource{ID: "file-id"}}
	require.NoError(t, o.Remove(context.Background()))
	empty = true
	require.NoError(t, f.Rmdir(context.Background(), "folder"))
	assert.Equal(t, 2, deletes)
	_, found := f.dirCache.Get("folder")
	assert.False(t, found)
}

func TestErrorsAndRetry(t *testing.T) {
	for _, status := range []int{401, 403, 404, 429, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			f, mux, _ := testFs(t, "")
			calls := 0
			mux.HandleFunc("GET /v1/drive/resources", func(w http.ResponseWriter, r *http.Request) {
				calls++
				if calls == 1 {
					http.Error(w, "error", status)
					return
				}
				jsonResponse(t, w, api.Listing{})
			})
			_, err := f.List(context.Background(), "")
			if status == 429 || status == 503 {
				require.NoError(t, err)
				assert.Equal(t, 2, calls)
			} else {
				require.Error(t, err)
				assert.Equal(t, 1, calls)
				if status == 404 {
					assert.ErrorIs(t, err, fs.ErrorDirNotFound)
				}
			}
		})
	}
}

func TestAboutAndConfig(t *testing.T) {
	f, mux, _ := testFs(t, "")
	mux.HandleFunc("GET /v1/drive/storage", func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(t, w, api.Storage{Total: 100, Used: 40})
	})
	usage, err := f.About(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int64(100), *usage.Total)
	assert.Equal(t, int64(40), *usage.Used)
	assert.Equal(t, int64(60), *usage.Free)
	_, err = NewFs(context.Background(), "test", "", configmap.Simple{})
	assert.ErrorContains(t, err, "access_token is required")
}

func TestResolveRoot(t *testing.T) {
	for _, root := range []string{"", "folder", "file", "missing", "missing/child"} {
		t.Run(root, func(t *testing.T) {
			f, mux, _ := testFs(t, root)
			mux.HandleFunc("GET /v1/drive/resources", func(w http.ResponseWriter, r *http.Request) {
				jsonResponse(t, w, api.Listing{Resources: []api.Resource{
					{ID: "folder-id", Name: "folder", Type: "folder"},
					{ID: "file-id", Name: "file", Type: "file"},
				}})
			})
			got, err := f.resolveRoot(context.Background())
			if root == "file" {
				assert.ErrorIs(t, err, fs.ErrorIsFile)
				assert.Equal(t, "", got.Root())
			} else {
				require.NoError(t, err)
				assert.Equal(t, root, got.Root())
			}
		})
	}
}

func TestRootPermissionError(t *testing.T) {
	f, mux, _ := testFs(t, "folder")
	mux.HandleFunc("GET /v1/drive/resources", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	})
	_, err := f.resolveRoot(context.Background())
	assert.ErrorContains(t, err, "403")
}

func TestCreateConflictAndEncoding(t *testing.T) {
	f, mux, _ := testFs(t, "")
	name := "invalid\xff"
	encoded := f.opt.Enc.FromStandardName(name)
	assert.NotEqual(t, name, encoded)
	mux.HandleFunc("POST /v1/drive/folders", func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&request))
		assert.Equal(t, encoded, request["folderName"])
		assert.NotContains(t, request, "parentId")
		http.Error(w, "exists", http.StatusConflict)
	})
	mux.HandleFunc("GET /v1/drive/resources", func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(t, w, api.Listing{Resources: []api.Resource{{ID: "existing", Name: encoded, Type: "folder"}}})
	})
	id, err := f.CreateDir(context.Background(), rootID, name)
	require.NoError(t, err)
	assert.Equal(t, "existing", id)
}

func TestCancellation(t *testing.T) {
	f, _, _ := testFs(t, "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := f.List(ctx, "")
	assert.ErrorIs(t, err, context.Canceled)
}

func TestRetryAfter(t *testing.T) {
	f, mux, _ := testFs(t, "")
	calls := 0
	mux.HandleFunc("GET /v1/drive/storage", func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "0")
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		jsonResponse(t, w, api.Storage{Total: 100, Used: 120})
	})
	usage, err := f.About(context.Background())
	require.NoError(t, err)
	assert.Zero(t, *usage.Free)
	assert.Equal(t, 2, calls)
}

func TestDirectoryRename(t *testing.T) {
	f, mux, _ := testFs(t, "")
	require.NotNil(t, f.Features().DirMove, "mount requires a server-side directory rename capability")
	ctx := context.Background()
	require.NoError(t, f.dirCache.FindRoot(ctx, false))
	f.dirCache.Put("old", "folder-id")
	f.dirCache.Put("old/child", "child-id")
	name := "old"
	mux.HandleFunc("GET /v1/drive/resources", func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(t, w, api.Listing{Resources: []api.Resource{{ID: "folder-id", Name: name, Type: "folder"}}})
	})
	mux.HandleFunc("POST /v1/drive/resources/folder-id/rename", func(w http.ResponseWriter, r *http.Request) {
		var request map[string]string
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&request))
		assert.Equal(t, "새 이름", request["name"])
		name = request["name"]
		jsonResponse(t, w, request)
	})
	require.NoError(t, f.Features().DirMove(ctx, f, "old", "새 이름"))
	_, found := f.dirCache.Get("old/child")
	assert.False(t, found, "descendant cache paths must be invalidated")
	id, err := f.dirCache.FindDir(ctx, "새 이름", false)
	require.NoError(t, err)
	assert.Equal(t, "folder-id", id)
}

func TestDirectoryRenameConflict(t *testing.T) {
	for _, status := range []int{403, 409} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			f, mux, _ := testFs(t, "")
			require.NotNil(t, f.Features().DirMove)
			ctx := context.Background()
			require.NoError(t, f.dirCache.FindRoot(ctx, false))
			f.dirCache.Put("old", "folder-id")
			mux.HandleFunc("GET /v1/drive/resources", func(w http.ResponseWriter, r *http.Request) {
				jsonResponse(t, w, api.Listing{Resources: []api.Resource{{ID: "folder-id", Name: "old", Type: "folder"}}})
			})
			mux.HandleFunc("POST /v1/drive/resources/folder-id/rename", func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "rename refused", status)
			})
			err := f.Features().DirMove(ctx, f, "old", "new")
			require.Error(t, err)
			if status == 409 {
				assert.ErrorIs(t, err, fs.ErrorDirExists)
			}
			id, found := f.dirCache.Get("old")
			assert.True(t, found)
			assert.Equal(t, "folder-id", id)
		})
	}
}

func TestFileRename(t *testing.T) {
	f, mux, _ := testFs(t, "")
	require.NotNil(t, f.Features().Move, "mount requires a server-side file rename capability")
	mux.HandleFunc("POST /v1/drive/resources/file-id/rename", func(w http.ResponseWriter, r *http.Request) {
		var request map[string]string
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&request))
		assert.Equal(t, "새 이름.txt", request["name"])
		jsonResponse(t, w, request)
	})
	src := &Object{fs: f, remote: "old.txt", item: api.Resource{ID: "file-id", Name: "old.txt", Size: 123}}
	dst, err := f.Features().Move(context.Background(), src, "새 이름.txt")
	require.NoError(t, err)
	assert.Equal(t, "새 이름.txt", dst.Remote())
	assert.Equal(t, "file-id", dst.(*Object).ID())
	assert.Equal(t, int64(123), dst.Size())
	assert.Equal(t, "old.txt", src.Remote())
}

func TestRenameRejectsCrossParent(t *testing.T) {
	f, _, _ := testFs(t, "")
	require.NotNil(t, f.Features().Move)
	require.NotNil(t, f.Features().DirMove)
	ctx := context.Background()
	src := &Object{fs: f, remote: "old.txt", item: api.Resource{ID: "file-id"}}
	_, err := f.Features().Move(ctx, src, "other/new.txt")
	assert.ErrorIs(t, err, fs.ErrorCantMove)
	err = f.Features().DirMove(ctx, f, "old", "other/new")
	assert.ErrorIs(t, err, fs.ErrorCantDirMove)
}

func TestLeadingPeriodUpload(t *testing.T) {
	f, mux, serverURL := testFs(t, "")
	mux.HandleFunc("POST /v1/drive/files", func(w http.ResponseWriter, r *http.Request) {
		var request api.UploadRequest
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&request))
		if request.Name != "．DS_Store" {
			http.Error(w, "leading dot rejected", http.StatusBadRequest)
			return
		}
		jsonResponse(t, w, api.Upload{URL: serverURL + "/upload"})
	})
	mux.HandleFunc("POST /upload", func(w http.ResponseWriter, r *http.Request) {
		assert.NoError(t, r.ParseMultipartForm(1024))
		file, header, err := r.FormFile("Filedata")
		if assert.NoError(t, err) {
			assert.Equal(t, "．DS_Store", header.Filename)
			assert.NoError(t, file.Close())
		}
		w.WriteHeader(http.StatusCreated)
	})
	mux.HandleFunc("GET /v1/drive/resources", func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(t, w, api.Listing{Resources: []api.Resource{{ID: "dot-file", Name: "．DS_Store", Type: "file", Size: 4}}})
	})
	src := object.NewStaticObjectInfo(".DS_Store", time.Now(), 4, true, nil, f)
	o, err := f.Put(context.Background(), strings.NewReader("test"), src)
	require.NoError(t, err)
	assert.Equal(t, ".DS_Store", o.Remote())
}
