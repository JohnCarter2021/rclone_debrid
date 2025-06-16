package torbox_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rclone/rclone/backend/torbox"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/fshttp"
	"github.com/rclone/rclone/fstest"
	"github.com/rclone/rclone/lib/rest" // Needed for direct Srv manipulation if setupTestFs changes
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testFs struct and newTestFsWithHandler are currently not used directly
// because setupTestFs handles Fs creation and server mocking.
// They are kept for potential future refactoring or alternative test setups.

// --- Global HTTP client override utilities for testing NewFs ---
// This is an invasive way to test NewFs without changing its signature.
// It should be used carefully and reset after each test.

var (
	originalDefaultTransport http.RoundTripper
	mockHandlerFunc          http.HandlerFunc
)

type mockRoundTripperForNewFs struct{}

func (mrt *mockRoundTripperForNewFs) RoundTrip(r *http.Request) (*http.Response, error) {
	if mockHandlerFunc != nil && (r.URL.Host == "api.torbox.app" || strings.HasPrefix(r.URL.Host, "127.0.0.1") || strings.HasPrefix(r.URL.Host, "localhost")) {
		// Simulate serving the request via a minimal httptest.Server-like interaction
		rr := httptest.NewRecorder()
		// Ensure the request URL path is what the handler expects (usually relative)
		// For this to work perfectly, the handler might need to be aware it's being called this way.
		// Or, ensure NewFs makes calls that this RoundTripper can identify and route to the handler.
		// The key is that r.URL.Path should match what the handler expects for /v1/api/user/me
		mockHandlerFunc(rr, r)
		return rr.Result(), nil
	}
	// Fallback to original transport if no mock handler or host doesn't match
	if originalDefaultTransport != nil {
		return originalDefaultTransport.RoundTrip(r)
	}
	return nil, fmt.Errorf("mockRoundTripperForNewFs: no mock handler or original transport for %s", r.URL.String())
}

func activateMockHTTPClient(t *testing.T, handler http.HandlerFunc) {
	if originalDefaultTransport != nil {
		t.Fatal("Mock HTTP client already active. Ensure deactivateMockHTTPClient is called.")
	}
	mockHandlerFunc = handler
	originalDefaultTransport = fshttp.DefaultClient.Transport // Assuming fshttp.DefaultClient is primarily used
	if originalDefaultTransport == nil {
		originalDefaultTransport = http.DefaultTransport // Fallback if fshttp.DefaultClient.Transport was nil
	}
	fshttp.DefaultClient.Transport = &mockRoundTripperForNewFs{}
}

func deactivateMockHTTPClient(t *testing.T) {
	if originalDefaultTransport == nil {
		t.Log("No mock HTTP client was active.")
		return
	}
	fshttp.DefaultClient.Transport = originalDefaultTransport
	originalDefaultTransport = nil
	mockHandlerFunc = nil
}

// --- Test Functions ---

func TestNewFs_Success(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/v1/api/user/me" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"user_id": 123, "username": "testuser", "email": "test@example.com", "storage_bytes_total": 10000, "storage_bytes_used": 2000}`))
			return
		}
		http.Error(w, fmt.Sprintf("TestNewFs_Success: Unhandled mock path: %s", r.URL.Path), http.StatusInternalServerError)
	}
	activateMockHTTPClient(t, handler)
	defer deactivateMockHTTPClient(t)

	ctx := context.Background()
	m := configmap.Simple{"api_token": "dummy_valid_token"}
	fObj, err := torbox.NewFs(ctx, "testremote", "", m)
	require.NoError(t, err)
	require.NotNil(t, fObj)

	f := fObj.(*torbox.Fs) // Cast to access Srv if needed, or other exported fields
	usage, err := f.About(ctx)
	require.NoError(t, err)

	total, okTotal := usage.Total.Get()
	used, okUsed := usage.Used.Get()
	free, okFree := usage.Free.Get()

	require.True(t, okTotal, "Total should be set")
	require.True(t, okUsed, "Used should be set")
	require.True(t, okFree, "Free should be set")

	assert.Equal(t, int64(10000), total)
	assert.Equal(t, int64(2000), used)
	assert.Equal(t, int64(8000), free)
}

func TestNewFs_AuthFailure(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/v1/api/user/me" {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		http.Error(w, fmt.Sprintf("TestNewFs_AuthFailure: Unhandled mock path: %s", r.URL.Path), http.StatusInternalServerError)
	}
	activateMockHTTPClient(t, handler)
	defer deactivateMockHTTPClient(t)

	ctx := context.Background()
	m := configmap.Simple{"api_token": "dummy_invalid_token"}
	_, err := torbox.NewFs(ctx, "testremote_authfail", "", m)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to connect to TorBox API") // Check for wrapped error
}

func TestNewFs_MissingToken(t *testing.T) {
	// No HTTP mock needed as it should fail before API call
	ctx := context.Background()
	m := configmap.Simple{"api_token": ""}
	_, err := torbox.NewFs(ctx, "testremote_notoken", "", m)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "API token not found")
}

// setupTestFs is a helper to create an Fs instance with its Srv pointing to a test server.
func setupTestFs(t *testing.T, initialHandler http.HandlerFunc) (*torbox.Fs, *httptest.Server, func()) {
	server := httptest.NewServer(initialHandler)
	cleanup := server.Close

	// This handler is for the NewFs call itself.
	activateMockHTTPClient(t, initialHandler)

	m := configmap.Simple{"api_token": "dummy-test-token"}
	fObj, err := torbox.NewFs(context.Background(), "testremote", "testroot", m)

	deactivateMockHTTPClient(t) // Important to deactivate after NewFs

	require.NoError(t, err, "NewFs failed during test setup")
	require.NotNil(t, fObj, "Fs object is nil after NewFs in test setup")

	f := fObj.(*torbox.Fs)
	// Replace the Fs.Srv with a new client pointing to the *same* test server
	// for subsequent calls within the test. The server's handler might need to be
	// updated by the test if subsequent calls hit different paths.
	newSrvClient := server.Client()
	f.Srv = rest.NewClient(fshttp.NewClientWithClient(context.Background(), newSrvClient)).SetRoot(server.URL)
	f.Srv.SetHeader("Authorization", "Bearer dummy-test-token") // Re-apply auth header

	return f, server, cleanup
}


func TestList_Root(t *testing.T) {
	mockHandler := func(w http.ResponseWriter, r *http.Request) {
		t.Logf("TestList_Root: Handler received %s %s", r.Method, r.URL.Path)
		if r.Method == "GET" && r.URL.Path == "/v1/api/user/me" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"user_id": 123, "email": "test@example.com", "storage_bytes_total": 1000, "storage_bytes_used": 200}`))
			return
		}
		if r.Method == "GET" && r.URL.Path == "/v1/api/torrents/mylist" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[
				{"id": 1, "hash": "hash1", "name": "Torrent One", "size_bytes": 1024, "created_at": "2023-01-01T12:00:00Z"},
				{"id": 2, "hash": "hash2", "name": "Torrent Two", "size_bytes": 2048, "created_at": "2023-01-02T13:00:00Z"}
			]`))
			return
		}
		http.Error(w, fmt.Sprintf("TestList_Root: Unhandled path %s", r.URL.Path), http.StatusNotFound)
	}

	f, _, cleanup := setupTestFs(t, mockHandler)
	defer cleanup()

	entries, err := f.List(context.Background(), "")
	require.NoError(t, err)
	require.Len(t, entries, 2)

	assert.Equal(t, "hash1", entries[0].Remote())
	assert.Equal(t, fs.EntryDirectory, entries[0].Type())
	assert.Equal(t, int64(1024), entries[0].Size())
	assert.Equal(t, time.Date(2023, 1, 1, 12, 0, 0, 0, time.UTC), entries[0].ModTime(context.Background()))

	assert.Equal(t, "hash2", entries[1].Remote())
	assert.Equal(t, fs.EntryDirectory, entries[1].Type())
	assert.Equal(t, int64(2048), entries[1].Size())
}


func TestList_TorrentContents(t *testing.T) {
	mockHandler := func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/v1/api/user/me" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"user_id": 123, "email": "test@example.com"}`))
			return
		}
		if r.Method == "GET" && r.URL.Path == "/v1/api/torrents/torrentinfo" {
			if r.URL.Query().Get("hash") == "test_hash" {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{
					"id": 10, "hash": "test_hash", "name": "Test Torrent", "size_bytes": 600,
					"created_at": "2023-01-03T10:00:00Z",
					"files": [
						{"id": 101, "path": "video.mp4", "name": "video.mp4", "size_bytes": 500},
						{"id": 102, "path": "docs/document.pdf", "name": "document.pdf", "size_bytes": 100}
					]
				}`))
				return
			}
		}
		http.Error(w, fmt.Sprintf("TestList_TorrentContents: Unhandled path %s", r.URL.Path), http.StatusNotFound)
	}
	f, _, cleanup := setupTestFs(t, mockHandler)
	defer cleanup()

	entries, err := f.List(context.Background(), "test_hash")
	require.NoError(t, err)
	require.Len(t, entries, 2)

	foundVideo, foundPdf := false, false
	for _, entry := range entries {
		obj, ok := entry.(fs.Object)
		require.True(t, ok)
		if obj.Remote() == "test_hash/video.mp4" {
			foundVideo = true
			assert.Equal(t, int64(500), obj.Size())
			assert.Equal(t, time.Date(2023, 1, 3, 10, 0, 0, 0, time.UTC), obj.ModTime(context.Background()))
		} else if obj.Remote() == "test_hash/document.pdf" {
			foundPdf = true
			assert.Equal(t, int64(100), obj.Size())
		}
	}
	assert.True(t, foundVideo, "video.mp4 not found")
	assert.True(t, foundPdf, "document.pdf not found")
}

func TestOpen_Success(t *testing.T) {
	var actualDownloadURL string // To capture the URL from requestdl mock
	mockHandler := func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/v1/api/user/me" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"user_id": 123}`))
			return
		}
		if r.Method == "GET" && r.URL.Path == "/v1/api/torrents/torrentinfo" { // For NewObject
			if r.URL.Query().Get("hash") == "dl_test_hash" {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{
					"id": 20, "hash": "dl_test_hash", "name": "Download Test", "created_at": "2023-01-01T00:00:00Z", "size_bytes": 11,
					"files": [{"id": 201, "path": "testfile.dat", "name": "testfile.dat", "size_bytes": 11}]
				}`))
				return
			}
		}
		if r.Method == "GET" && r.URL.Path == "/v1/api/torrents/requestdl" { // For Open
			assert.Equal(t, "dummy-test-token", r.URL.Query().Get("token"))
			assert.Equal(t, "20", r.URL.Query().Get("torrent_id"))
			assert.Equal(t, "201", r.URL.Query().Get("file_id"))

			actualDownloadURL = "/downloads/actual_file.dat"
			respJSON := fmt.Sprintf(`{"url": "%s%s"}`, "http://"+r.Host, actualDownloadURL)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(respJSON))
			return
		}
		if actualDownloadURL != "" && r.Method == "GET" && r.URL.Path == actualDownloadURL { // Actual download
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write([]byte("hello world"))
			return
		}
		http.Error(w, fmt.Sprintf("TestOpen_Success: Unhandled path %s or query %s", r.URL.Path, r.URL.RawQuery), http.StatusNotFound)
	}

	f, server, cleanup := setupTestFs(t, mockHandler)
	defer cleanup()

	// The Fs.Srv is already pointing to the test server by setupTestFs for subsequent calls.
	// We just need to ensure the handler in the server can handle all expected calls.
	// We update the server's handler to ensure it can serve the download URL.
	server.Config.Handler = http.HandlerFunc(mockHandler)


	obj, err := f.NewObject(context.Background(), "dl_test_hash/testfile.dat")
	require.NoError(t, err)
	require.NotNil(t, obj)
	assert.Equal(t, int64(11), obj.Size())

	rc, err := obj.Open(context.Background())
	require.NoError(t, err)
	defer func() { _ = rc.Close() }()

	content, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, "hello world", string(content))
}


func TestPurge_Success(t *testing.T) {
	mockHandler := func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/v1/api/user/me" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"user_id": 123}`))
			return
		}
		if r.Method == "GET" && r.URL.Path == "/v1/api/torrents/torrentinfo" && r.URL.Query().Get("hash") == "purge_hash" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id": 30, "hash": "purge_hash", "name": "Torrent to Purge", "size_bytes": 0, "created_at": "2023-01-01T00:00:00Z", "files":[]}`))
			return
		}
		if r.Method == "POST" && r.URL.Path == "/v1/api/torrents/controltorrent" {
			bodyBytes, _ := io.ReadAll(r.Body)
			var payload torbox.ControlTorrentPayload
			err := json.Unmarshal(bodyBytes, &payload)
			require.NoError(t, err, "Failed to unmarshal payload")
			assert.Equal(t, int64(30), payload.TorrentID)
			assert.Equal(t, "delete", payload.Operation)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status": "success"}`))
			return
		}
		http.Error(w, fmt.Sprintf("TestPurge_Success: Unhandled path %s", r.URL.Path), http.StatusNotFound)
	}
	f, server, cleanup := setupTestFs(t, mockHandler)
	defer cleanup()
	server.Config.Handler = http.HandlerFunc(mockHandler) // Ensure server uses the full handler

	err := f.Purge(context.Background(), "purge_hash")
	assert.NoError(t, err)
}

func TestRmdir_Success(t *testing.T) {
	mockHandler := func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/v1/api/user/me" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"user_id": 123}`))
			return
		}
		if r.Method == "GET" && r.URL.Path == "/v1/api/torrents/torrentinfo" && r.URL.Query().Get("hash") == "rmdir_hash" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id": 40, "hash": "rmdir_hash", "name": "Torrent to Rmdir", "size_bytes":0, "created_at":"2023-01-01T00:00:00Z", "files":[]}`))
			return
		}
		if r.Method == "POST" && r.URL.Path == "/v1/api/torrents/controltorrent" {
			bodyBytes, _ := io.ReadAll(r.Body)
			var payload torbox.ControlTorrentPayload
			err := json.Unmarshal(bodyBytes, &payload)
			require.NoError(t, err)
			assert.Equal(t, int64(40), payload.TorrentID)
			assert.Equal(t, "delete", payload.Operation)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status": "success"}`))
			return
		}
		http.Error(w, fmt.Sprintf("TestRmdir_Success: Unhandled path %s", r.URL.Path), http.StatusNotFound)
	}
	f, server, cleanup := setupTestFs(t, mockHandler)
	defer cleanup()
	server.Config.Handler = http.HandlerFunc(mockHandler)

	err := f.Rmdir(context.Background(), "rmdir_hash")
	assert.NoError(t, err)
}

func TestPut_TorrentFile(t *testing.T) {
	mockHandler := func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/v1/api/user/me" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"user_id": 123}`))
			return
		}
		if r.Method == "POST" && r.URL.Path == "/v1/api/torrents/createtorrent" {
			err := r.ParseMultipartForm(32 << 20)
			require.NoError(t, err)

			file, fh, err := r.FormFile("file")
			require.NoError(t, err)
			defer func() { _ = file.Close() }()
			assert.Equal(t, "local.torrent", fh.Filename)
			assert.Equal(t, "local.torrent", r.FormValue("name"))

			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id": 50, "hash": "new_torrent_hash", "name": "local.torrent"}`))
			return
		}
		http.Error(w, fmt.Sprintf("TestPut_TorrentFile: Unhandled path %s", r.URL.Path), http.StatusNotFound)
	}
	f, server, cleanup := setupTestFs(t, mockHandler)
	defer cleanup()
	server.Config.Handler = http.HandlerFunc(mockHandler)

	dummyTorrentContent := []byte("dummy torrent file content")
	src := fstest.NewObject("local.torrent", time.Now(), dummyTorrentContent)

	obj, err := f.Put(context.Background(), bytes.NewReader(dummyTorrentContent), src)
	assert.NoError(t, err)
	assert.Nil(t, obj)
}

func TestPut_MagnetLink(t *testing.T) {
	magnet := "magnet:?xt=urn:btih:somehashvalue"
	mockHandler := func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/v1/api/user/me" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"user_id": 123}`))
			return
		}
		if r.Method == "POST" && r.URL.Path == "/v1/api/torrents/createtorrent" {
			require.NoError(t, r.ParseForm())
			assert.Equal(t, magnet, r.FormValue("magnet"))
			assert.Equal(t, "myfave.magnet", r.FormValue("name"))

			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id": 60, "hash": "magnet_hash", "name": "Magnet Torrent Name"}`))
			return
		}
		http.Error(w, fmt.Sprintf("TestPut_MagnetLink: Unhandled path %s", r.URL.Path), http.StatusNotFound)
	}
	f, server, cleanup := setupTestFs(t, mockHandler)
	defer cleanup()
	server.Config.Handler = http.HandlerFunc(mockHandler)

	src := fstest.NewObject("myfave.magnet", time.Now(), []byte(magnet))

	obj, err := f.Put(context.Background(), strings.NewReader(magnet), src)
	assert.NoError(t, err)
	assert.Nil(t, obj)
}

// TestPut_UnsupportedFile, TestList_NonExistentTorrent, TestNewObject_NotFound
// can remain largely the same as their core logic (error checking)
// doesn't heavily depend on the exact structure of successful responses,
// but their /user/me mocks should align for setupTestFs.

func TestPut_UnsupportedFile(t *testing.T) {
    handler := func(w http.ResponseWriter, r *http.Request) {
        if r.Method == "GET" && r.URL.Path == "/v1/api/user/me" {
            w.Header().Set("Content-Type", "application/json")
            _, _ = w.Write([]byte(`{"user_id": 123}`)) // For setupTestFs
            return
        }
        http.Error(w, "TestPut_UnsupportedFile: This should not be called for unsupported files", http.StatusInternalServerError)
    }

    f, _, cleanup := setupTestFs(t, handler)
    defer cleanup()

    randomContent := []byte("this is not a torrent or magnet")
    src := fstest.NewObject("myfile.zip", time.Now(), randomContent)

    _, err := f.Put(context.Background(), bytes.NewReader(randomContent), src)
    require.Error(t, err)
    assert.Contains(t, err.Error(), "TorBox backend only supports adding .torrent files or magnet links")
}

func TestList_NonExistentTorrent(t *testing.T) {
    handler := func(w http.ResponseWriter, r *http.Request) {
        if r.Method == "GET" && r.URL.Path == "/v1/api/user/me" {
            w.Header().Set("Content-Type", "application/json")
            _, _ = w.Write([]byte(`{"user_id": 123, "email": "test@example.com"}`))
            return
        }
        if r.Method == "GET" && r.URL.Path == "/v1/api/torrents/torrentinfo" {
            if r.URL.Query().Get("hash") == "non_existent_hash" {
                http.Error(w, "Torrent not found", http.StatusNotFound) // getTorrentInfoByHash will error
                return
            }
        }
        http.Error(w, fmt.Sprintf("TestList_NonExistentTorrent: Unhandled path %s", r.URL.Path), http.StatusInternalServerError)
    })

    f, _, cleanup := setupTestFs(t, handler)
    defer cleanup()

    _, err := f.List(context.Background(), "non_existent_hash")
    require.Error(t, err)
    // This error comes from getTorrentInfoByHash failing and List propagating it.
    // If getTorrentInfoByHash maps to fs.ErrorObjectNotFound, then List might map to fs.ErrorDirNotFound.
    // Current torbox.go maps "received empty or invalid torrent info" to fs.ErrorDirNotFound in List.
    // A StatusNotFound from http.Error will likely result in a generic "failed to get torrent info" or similar.
    assert.Contains(t, err.Error(), "failed to get torrent info")
}

func TestNewObject_NotFound(t *testing.T) {
    handler := func(w http.ResponseWriter, r *http.Request) {
        if r.Method == "GET" && r.URL.Path == "/v1/api/user/me" {
            w.Header().Set("Content-Type", "application/json")
            _, _ = w.Write([]byte(`{"user_id": 123}`))
            return
        }
        if r.Method == "GET" && r.URL.Path == "/v1/api/torrents/torrentinfo" {
            queryHash := r.URL.Query().Get("hash")
            if queryHash == "bad_hash" {
                http.Error(w, "Torrent not found", http.StatusNotFound) // getTorrentInfoByHash will error
                return
            }
            if queryHash == "good_hash_no_file" {
                w.Header().Set("Content-Type", "application/json")
                // Torrent exists, but file won't be in "files"
                _, _ = w.Write([]byte(`{"id":70, "hash":"good_hash_no_file", "name":"Good Torrent", "files":[]}`))
                return
            }
        }
        http.Error(w, fmt.Sprintf("TestNewObject_NotFound: Unhandled path %s", r.URL.Path), http.StatusInternalServerError)
    })

    f, _, cleanup := setupTestFs(t, handler)
    defer cleanup()

    _, err := f.NewObject(context.Background(), "bad_hash/file.txt")
    require.Error(t, err)
    assert.Contains(t, err.Error(), "failed to get torrent info")

    _, err = f.NewObject(context.Background(), "good_hash_no_file/non_existent_file.txt")
    require.Error(t, err)
    assert.Equal(t, fs.ErrorObjectNotFound, err) // Should be specific if torrent found but file not.
}

// Note: The `setupTestFs` helper was refactored to use a global HTTP client override (`activateMockHTTPClient`)
// for the NewFs call, and then replace Fs.Srv for subsequent calls. This is still complex.
// Ideal solution: torbox.NewFs accepts *http.Client or a base URL.
// The Object.Name() assertion style `obj.(*torbox.Object).Name()` would require tests to be in `package torbox`
// or for `Name` to be an exported field or method of `torbox.Object`. For `fs.Object` interface, only `Remote()` is standard.
// Assertions for specific struct fields (e.g. torrentID, fileID on Object) are not done as these are internal details
// not exposed by fs.Object or fs.DirEntry, but their correct usage is implicitly tested by params sent to mock APIs.
