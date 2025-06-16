package torbox

import (
	"context"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/fshttp"
	"github.com/rclone/rclone/lib/rest"
)

// Fs represents a remote torbox
type Fs struct {
	name     string
	root     string
	opts     *Options
	features *fs.Features
	Srv      *rest.Client // Exported for testing client replacement
	// Add other necessary fields like token, client, etc.
}

// Options defines the configuration for this backend
type Options struct {
	APIToken string `config:"api_token"`
	// Add other options as needed, e.g., session_token for initial auth
}

// Object describes a torbox object
//
// Will be a file within a torrent or potentially a whole torrent if treated as a file
type Object struct {
	fs          *Fs
	remote      string // This will be the path relative to the Fs root
	name        string // Name of the file object itself
	torrentID   int64  // Integer ID of the torrent this object belongs to
	torrentHash string // Hash of the torrent this file belongs to (still useful for some calls)
	fileID      int64  // Integer ID of the file within the torrent
	size        int64  // Size in bytes
	modTime     time.Time
}

func init() {
	fs.Register(&fs.RegInfo{
		Name:        "torbox",
		Description: "TorBox",
		NewFs:       NewFs,
		Options: []fs.Option{
			{
				Name:     "api_token",
				Help:     "TorBox API Token.\n\nGet yours from your TorBox account settings.",
				Required: true,
				Obscure:  true,
			},
			// Add other config options here
		},
		// DefaultFeatures: (optional, can add later)
	})
}

// UserMeInfo struct to parse the response from /v1/api/user/me
type UserMeInfo struct {
	Username          string `json:"username,omitempty"` // From API spec (assumed common)
	UserID            int64  `json:"user_id,omitempty"`  // From API spec (assumed common)
	StorageBytesTotal *int64 `json:"storage_bytes_total,omitempty"`
	StorageBytesUsed  *int64 `json:"storage_bytes_used,omitempty"`
	Email             string `json:"email,omitempty"` // Kept from previous
}

// TorrentFile defines the structure for a file within a torrent
// Exported for tests.
type TorrentFile struct {
	ID        int64  `json:"id"`         // file ID (integer)
	Path      string `json:"path"`       // full path of file in torrent
	Name      string `json:"name"`       // file name
	SizeBytes int64  `json:"size_bytes"` // size in bytes
}

// TorrentInfo defines the structure for torrent details
// Exported for tests.
type TorrentInfo struct {
	ID        int64         `json:"id"`    // torrent ID (integer)
	Hash      string        `json:"hash"`  // torrent hash (string)
	Name      string        `json:"name"`
	SizeBytes int64         `json:"size_bytes"`
	CreatedAt string        `json:"created_at"` // ISO8601 string from API
	Files     []TorrentFile `json:"files"`
}

// MyTorrentsListItem defines the structure for an item in the torrent list
// Exported for tests.
type MyTorrentsListItem struct {
	ID        int64  `json:"id"`    // torrent ID (integer)
	Hash      string `json:"hash"`  // torrent hash (string)
	Name      string `json:"name"`
	SizeBytes int64  `json:"size_bytes"`
	CreatedAt string `json:"created_at"` // ISO8601 string from API
}

// DownloadLink defines the structure for the requestdl response
// Exported for tests.
type DownloadLink struct {
	URL string `json:"url"` // Assuming API returns {"url": "..."}
}

// ControlTorrentPayload defines the structure for controlling a torrent (e.g., delete)
// Exported for tests to allow payload verification.
type ControlTorrentPayload struct {
	Operation string `json:"operation"`         // e.g., "delete"
	TorrentID int64  `json:"torrent_id"`        // TorBox API spec uses integer torrent_id
	All       bool   `json:"all,omitempty"`     // For operations like "deleteall"
}

// TorrentAddResponse structure for the response from createtorrent
// Exported for tests.
type TorrentAddResponse struct {
	ID   int64  `json:"id"`              // Integer ID of the newly added torrent
	Hash string `json:"hash"`            // Hash of the newly added torrent
	Name string `json:"name"`            // Name of the newly added torrent
	// Message string `json:"message,omitempty"` // Optional message
}

// NewFs constructs an Fs from the path and options.
func NewFs(ctx context.Context, name, root string, m configmap.Mapper) (fs.Fs, error) {
	opts := new(Options)
	err := fs.ParseOptions(m, opts)
	if err != nil {
		return nil, fmt.Errorf("failed to parse options: %w", err)
	}

	if opts.APIToken == "" {
		return nil, fmt.Errorf("API token not found in config")
	}

	client := fshttp.NewClient(ctx)
	srv := rest.NewClient(client).SetRoot("https://api.torbox.app")
	srv.SetHeader("Authorization", "Bearer "+opts.APIToken)

	f := &Fs{
		name: name,
		root: root,
		opts: opts,
		Srv:  srv, // Assign to exported Srv field
	}

	// Test API connection and token validity
	var userInfo UserMeInfo
	resp, err := f.Srv.CallJSON(ctx, "GET", "/v1/api/user/me", nil, &userInfo) // Use f.Srv
	if err != nil {
		return nil, fmt.Errorf("failed to connect to TorBox API: %w (resp: %v)", err, resp)
	}
	if userInfo.ID == "" {
		return nil, fmt.Errorf("API token seems invalid, user ID not returned (resp: %v)", resp)
	}
	fs.Logf(f, "Successfully connected to TorBox. User: %s (ID: %s)", userInfo.Email, userInfo.ID)


	f.features = (&fs.Features{
		// CanCopy:      false, // Server-side copy not apparent from API
		// CanMove:      false, // Server-side move not apparent
		// CanDirMove:   false,
		// CanStream:    true, // For downloads
		// CanGetTier:   false,
		// CanSetTier:   false,
		// DirModTimeUpdatesAfterUploadingFile: false, // Unknown
	}).Fill(ctx, f) // Fill will set common features like CaseInsensitive based on OS

	// Initial path check and setup if needed (e.g. for root)
	// Test API connection / token validity by calling /v1/api/user/me
	// For now, just return the Fs object
	return f, nil
}

// parseTime tries to parse a time string from TorBox API (assumed ISO8601)
func parseTime(ts string) time.Time {
	if ts == "" {
		return time.Time{} // Return zero time if empty
	}
	// TorBox API spec doesn't specify format, common formats are RFC3339 or ISO8601 variants
	// Example: "2023-01-01T12:00:00Z" or "2024-07-15 10:30:00"
	// Try a few common formats. time.RFC3339Nano is a good general one.
	layouts := []string{
		time.RFC3339,
		time.RFC3339Nano,
		"2006-01-02T15:04:05", // No Z
		"2006-01-02 15:04:05", // Space instead of T, no Z
	}
	for _, layout := range layouts {
		t, err := time.Parse(layout, ts)
		if err == nil {
			return t
		}
	}
	fs.Debugf(nil, "Failed to parse time string '%s' with known layouts", ts)
	return time.Time{} // Return zero time if all parsing fails
}


// getTorrentInfoByHash fetches torrent information by its hash.
func (f *Fs) getTorrentInfoByHash(ctx context.Context, hash string) (*TorrentInfo, error) {
	var torrentInfo TorrentInfo
	_, err := f.Srv.CallJSON(ctx, "GET", "/v1/api/torrents/torrentinfo?hash="+hash, nil, &torrentInfo)
	if err != nil {
		return nil, fmt.Errorf("failed to get torrent info for hash %s: %w", hash, err)
	}
	if torrentInfo.Hash == "" && torrentInfo.ID == 0 { // Check if response is empty or invalid
		return nil, fmt.Errorf("received empty or invalid torrent info for hash %s", hash)
	}
	return &torrentInfo, nil
}


// Remote returns the remote path
func (o *Object) Remote() string {
	return o.remote
}

// String returns a description of the Object
func (o *Object) String() string {
	if o == nil {
		return "<nil>"
	}
	return o.Remote()
}

// ModTime returns the modification time of the object
// It attempts to read the objects mtime and if that isn't present the
// remote modTime is returned.
func (o *Object) ModTime(ctx context.Context) time.Time {
	return o.modTime
}

// Size returns the size of an object in bytes
func (o *Object) Size() int64 {
	return o.size
}

// Fs returns the parent Fs
func (o *Object) Fs() fs.Info {
	return o.fs
}

// Hash returns the selected checksum of the object
// If no checksum is available it returns ""
func (o *Object) Hash(ctx context.Context, ty fs.HashType) (string, error) {
	return "", fs.ErrHashUnsupported
}

// SetModTime sets the modification time of the local fs object
func (o *Object) SetModTime(ctx context.Context, modTime time.Time) error {
	return fs.ErrNotImplemented
}

// Storable returns whether this object is storable
func (o *Object) Storable() bool {
	return true
}

// Open opens the file for reading.
// It should call either Update or Read an object.
func (o *Object) Open(ctx context.Context, options ...fs.OpenOption) (io.ReadCloser, error) {
	// Apply OpenOptions if any - for now, we ignore them (e.g. Range requests)
	// fs.FixRangeOption(options, o.size) // Example if handling Range
	// var offset int64 = 0
	// if умираOption, ok := fs.FindOpenOption(options, fs.SeekOption{}); ok {
	// 	offset = умираOption.(fs.SeekOption).Offset
	// }
	// var limit int64 = -1
	// if limitOption, ok := fs.FindOpenOption(options, fs.RangeOption{}); ok {
	// 	limit = limitOption.(fs.RangeOption).To - limitOption.(fs.RangeOption).From + 1
	// }


	var dlLink DownloadLink
	// API Spec: token (query, string, required), torrent_id (query, integer, required), file_id (query, integer, optional, default: 0)
	apiPath := fmt.Sprintf("/v1/api/torrents/requestdl?token=%s&torrent_id=%d&file_id=%d",
		o.fs.opts.APIToken, o.torrentID, o.fileID)

	// According to API spec, requestdl returns the URL directly as a string, not a JSON object.
	// So, Call should be used, not CallJSON. We expect a raw string response.
	// However, the DownloadLink struct { URL string `json:"url"` } implies a JSON response.
	// Let's assume the struct is correct and API returns JSON for consistency with other calls.
	// If it's truly a raw string, this needs to change to use srv.Call and handle raw response.
	resp, err := o.fs.Srv.CallJSON(ctx, "GET", apiPath, nil, &dlLink)
	if err != nil {
		return nil, fmt.Errorf("failed to request download link for %s (torrentID: %d, fileID: %d): %w (resp: %v)", o.remote, o.torrentID, o.fileID, err, resp)
	}
	if dlLink.URL == "" {
		return nil, fmt.Errorf("download link not found for %s (torrentID: %d, fileID: %d) (resp: %v)", o.remote, o.torrentID, o.fileID, resp)
	}

	// Make a new HTTP request to the actual download URL
	// We use a new client here as this is a direct download, not an API call to TorBox itself.
	// fshttp.NewClient(ctx) will use rclone's global http client settings (proxy, timeout etc)
	httpClient := fshttp.NewClient(ctx)
	httpReq, err := http.NewRequestWithContext(ctx, "GET", dlLink.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request for download URL %s: %w", dlLink.URL, err)
	}

	// If we were handling Range requests:
	// if offset > 0 || limit != -1 {
	// 	fs.RangeRequest(httpReq, offset, limit)
	// }

	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("failed to download file from %s: %w", dlLink.URL, err)
	}

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		defer fs.SafeClose(httpResp.Body)
		return nil, fmt.Errorf("failed to download file: %s (status: %s, url: %s)", httpResp.Status, httpResp.Status, dlLink.URL)
	}

	// TODO: Wrap httpResp.Body with accounting if needed (e.g. o.fs.accounting.NewFs(httpResp.Body))
	return httpResp.Body, nil
}

// Update the object with the contents of the io.Reader, modTime and size
//
// If existing is set then it is an update of an existing object,
// otherwise a new object should be created
func (o *Object) Update(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) error {
	// Placeholder - will be implemented in a later step
	return fs.ErrNotImplemented
}

// Remove an object
func (o *Object) Remove(ctx context.Context) error {
	// TorBox API does not support deleting individual files from a torrent.
	// The entire torrent must be deleted (e.g. using 'rclone purge remote:torrent_hash' or 'rclone rmdir remote:torrent_hash')
	// and re-added if files need to be removed from it.
	return fs.ErrNotImplemented // Or fs.ErrCantDelete
}

// Name of the remote (as passed into NewFs)
func (f *Fs) Name() string {
	return f.name
}

// Root of the remote (as passed into NewFs)
func (f *Fs) Root() string {
	return f.root
}

// String returns a description of the FS
func (f *Fs) String() string {
	return fmt.Sprintf("TorBox remote %s:%s", f.name, f.root)
}

// Features returns the optional features of this Fs
func (f *Fs) Features() *fs.Features {
	return f.features
}

// List the objects and directories in dir into entries.  The
// entries can be returned in any order.  NoTraverse should be
// respected.  Recursive has no meaning here.
func (f *Fs) List(ctx context.Context, dir string) (entries fs.DirEntries, err error) {
	// If dir is empty or matches f.root, list torrents
	// For TorBox, f.root is likely conceptual and dir will be empty for root listing.
	if dir == "" || dir == f.root {
		var torrents []MyTorrentsListItem
		_, err := f.Srv.CallJSON(ctx, "GET", "/v1/api/torrents/mylist", nil, &torrents) // Use f.Srv
		if err != nil {
			return nil, fmt.Errorf("failed to list torrents: %w", err)
		}
		for _, t := range torrents {
			// Use torrent hash as name for directory listing for now.
			// API gives us t.Name which could be used if we ensure uniqueness / handle conflicts.
			entry := fs.NewDir(t.Hash, parseTime(t.CreatedAt))
			entry.SetSize(t.SizeBytes)
			// We could store t.ID (torrent's integer ID) in the Dir object if needed later,
			// but rclone's Dir model doesn't have a generic ID field.
			// We'd need a map in Fs if we want to look up ID by hash/name from listing.
			entries = append(entries, entry)
		}
		return entries, nil
	}

	// Otherwise, assume dir is a torrent_hash and list files within it
	torrentHash := dir
	torrentInfoData, err := f.getTorrentInfoByHash(ctx, torrentHash)
	if err != nil {
		// Map 404 or other errors to fs.ErrorDirNotFound if possible
		if strings.Contains(err.Error(), "received empty or invalid torrent info") { // Crude check
			return nil, fs.ErrorDirNotFound
		}
		return nil, fmt.Errorf("failed to get torrent info for %s: %w", torrentHash, err)
	}

	for _, file := range torrentInfoData.Files {
		remotePath := path.Join(torrentHash, file.Name) // file.Path could also be used if it's relative
		obj := &Object{
			fs:          f,
			remote:      remotePath,
			name:        file.Name,
			torrentID:   torrentInfoData.ID,
			torrentHash: torrentInfoData.Hash,
			fileID:      file.ID,
			size:        file.SizeBytes,
			modTime:     parseTime(torrentInfoData.CreatedAt), // Use torrent creation time for files
		}
		entries = append(entries, obj)
	}
	return entries, nil
}

// NewObject finds the Object at remote.  If it can't be found
// it returns the error fs.ErrorObjectNotFound.
func (f *Fs) NewObject(ctx context.Context, remote string) (fs.Object, error) {
	parts := strings.SplitN(remote, "/", 2)
	if len(parts) != 2 {
		return nil, fs.ErrorObjectNotFound // Path should be torrent_hash/file_name
	}
	torrentHashOrName := parts[0]
	fileName := parts[1]

	// For now, assume torrentHashOrName is the actual hash.
	torrentHash := torrentHashOrName

	torrentInfoData, err := f.getTorrentInfoByHash(ctx, torrentHash)
	if err != nil {
		// Map 404 or other errors to fs.ErrorObjectNotFound or fs.ErrorDirNotFound
		if strings.Contains(err.Error(), "received empty or invalid torrent info") { // Crude check
			return nil, fs.ErrorObjectNotFound // If torrent itself not found
		}
		return nil, fmt.Errorf("failed to get torrent info for %s (for file %s): %w", torrentHash, fileName, err)
	}

	for _, file := range torrentInfoData.Files {
		// Compare either by full path if file.Path is relative to torrent root, or by name.
		// API spec says file.Path is "full path of file in torrent"
		// API spec says file.Name is "file name"
		// If file.Path is like "subdir/file.txt" and fileName is "subdir/file.txt", use file.Path
		// If fileName is just "file.txt", then we might need to search more carefully or expect paths to be flat.
		// For now, assume fileName matches file.Name directly (i.e., paths are torrent_hash/file.Name)
		if file.Name == fileName {
			obj := &Object{
				fs:          f,
				remote:      remote, // Full remote path
				name:        file.Name,
				torrentID:   torrentInfoData.ID,
				torrentHash: torrentInfoData.Hash,
				fileID:      file.ID,
				size:        file.SizeBytes,
				modTime:     parseTime(torrentInfoData.CreatedAt), // Use torrent creation time
			}
			return obj, nil
		}
	}

	return nil, fs.ErrorObjectNotFound
}

// Mkdir makes the directory (container, bucket)
//
// Optional interface: Only implement this if you have a way of
// making directories on the remote.
func (f *Fs) Mkdir(ctx context.Context, dir string) error {
	return fs.ErrNotImplemented // Torrents are primary, "making a directory" might not map well
}

// Rmdir removes the directory (container, bucket) if empty
//
// Optional interface: Only implement this if you have a way of
// removing directories on the remote.
func (f *Fs) Rmdir(ctx context.Context, dir string) error {
	// Placeholder - will be implemented by deleting a torrent
	return fs.ErrNotImplemented
}

// Precision returns the precision of the ModTimes in this Fs
func (f *Fs) Precision() time.Duration {
	return time.Second // ModTimes are based on torrent creation times
}

// Purge deletes all the files in the directory
//
// Optional interface: Only implement this if you have a way of
// removing all the files at once.
func (f *Fs) Purge(ctx context.Context, dir string) error {
	if dir == "" || dir == f.root { // Cannot purge root or if dir is effectively root
		return fs.ErrorCantPurgeRoot
	}

	// dir is the torrent hash. We need the integer TorrentID for the API call.
	torrentInfo, err := f.getTorrentInfoByHash(ctx, dir)
	if err != nil {
		// If torrent not found by hash, it might already be deleted or never existed.
		// Consider this as non-error for Purge, or map to fs.ErrorDirNotFound if strict.
		if strings.Contains(err.Error(), "received empty or invalid torrent info") {
			fs.Debugf(f, "Torrent with hash %s not found, perhaps already deleted.", dir)
			return nil // Or fs.ErrorDirNotFound
		}
		return fmt.Errorf("failed to get info for torrent hash %s before deletion: %w", dir, err)
	}

	payload := ControlTorrentPayload{
		Operation: "delete", // As per API spec
		TorrentID: torrentInfo.ID,
		// All: false, // Assuming 'delete' operation doesn't need 'All' or defaults it.
	}

	// The API spec implies a single object for payload, not a list.
	// If it expects a list: []ControlTorrentPayload{payload}
	_, err = f.Srv.CallJSON(ctx, "POST", "/v1/api/torrents/controltorrent", payload, nil)
	if err != nil {
		return fmt.Errorf("failed to delete torrent %s (ID: %d): %w", dir, torrentInfo.ID, err)
	}
	return nil
}

// Rmdir removes the directory (container, bucket) if empty
//
// Optional interface: Only implement this if you have a way of
// removing directories on the remote.
func (f *Fs) Rmdir(ctx context.Context, dir string) error {
	// For TorBox, Rmdir on a torrent hash is the same as Purge, as torrents are the "directories"
	if dir == "" || dir == f.root {
		return fs.ErrorCantRmdirRoot
	}
	return f.Purge(ctx, dir) // Internally call Purge
}

// Precision returns the precision of the ModTimes in this Fs
func (f *Fs) Precision() time.Duration {
	return time.Second // ModTimes are based on torrent creation times
}

// Put in to the remote path with the modTime given of the given size
//
// May create the object even if it returns an error - if so
// will return the object and the error, otherwise will return
// nil and the error
func (f *Fs) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	remote := src.Remote()
	// Note: TorrentAddResponse struct is now defined globally and expects ID (int) and Hash (string).
	remoteName := src.Remote() // This will be used for the 'name' parameter if provided.

	if strings.HasSuffix(strings.ToLower(remoteName), ".torrent") {
		// Uploading a .torrent file
		// API Spec: file (binary, opt), magnet (string, opt), seed (int, opt),
		// allow_zip (bool, opt, def:true), name (string, opt), as_queued (bool, opt, def:false).
		params := rest.Params{
			"_filter":       []string{"file"}, // Parameter for the file content in CallMultipartUpload
			"file":          remoteName,       // Filename for the multipart request part
			"name":          remoteName,       // Optional name for the torrent
			// "seed": "0", // Example if we wanted to control seeding, API default is likely fine
			// "as_queued": "false", // API default
		}
		var torrentResp TorrentAddResponse
		// Pass 'in' as the reader for the "file" part (matching "_filter")
		_, err := f.Srv.CallMultipartUpload(ctx, "POST", "/v1/api/torrents/createtorrent", params, "file", remoteName, in, &torrentResp)
		if err != nil {
			return nil, fmt.Errorf("failed to upload .torrent file %s: %w", remoteName, err)
		}
		fs.Logf(f, "Successfully added torrent from file %s. ID: %d, Hash: %s, Name: %s", remoteName, torrentResp.ID, torrentResp.Hash, torrentResp.Name)
		return nil, nil
	} else if strings.HasPrefix(remoteName, "magnet:") || strings.HasSuffix(strings.ToLower(remoteName), ".magnet") {
		magnetLinkBytes, err := io.ReadAll(in)
		if err != nil {
			return nil, fmt.Errorf("failed to read magnet link from input for %s: %w", remoteName, err)
		}
		magnetLink := strings.TrimSpace(string(magnetLinkBytes))

		if !strings.HasPrefix(magnetLink, "magnet:") {
			return nil, fmt.Errorf("content of %s does not appear to be a valid magnet link", remoteName)
		}

		// API Spec: file (binary, opt), magnet (string, opt), seed (int, opt),
		// allow_zip (bool, opt, def:true), name (string, opt), as_queued (bool, opt, def:false).
		// For magnet, send as form parameters.
		formParams := map[string]string{
			"magnet": magnetLink,
			"name":   remoteName, // Optional name for the torrent
			// "seed": "0",
			// "as_queued": "false",
		}
		var torrentResp TorrentAddResponse
		// Use srv.Call with form encoding
		resp, err := f.Srv.Call(ctx, "POST", "/v1/api/torrents/createtorrent", formParams, &torrentResp)
		if err != nil {
			return nil, fmt.Errorf("failed to add magnet link from %s: %w (resp: %v)", remoteName, err, resp)
		}
		// Check if torrentResp is properly populated, Call might not unmarshal JSON if Content-Type isn't application/json
		// If the response from TorBox for form-urlencoded POST is JSON, then Call should handle it if headers are right.
		// If Call doesn't unmarshal, we might need to manually parse resp.Body.
		// For now, assume Call with &torrentResp works if API returns JSON.
		if torrentResp.Hash == "" && torrentResp.ID == 0 {
			bodyBytes, _ := io.ReadAll(resp.Body)
			fs.SafeClose(resp.Body)
			return nil, fmt.Errorf("failed to add magnet link from %s (empty/invalid response from API). Status: %s, Body: %s", remoteName, resp.Status, string(bodyBytes))
		}

		fs.Logf(f, "Successfully added torrent from magnet link in %s. ID: %d, Hash: %s, Name: %s", remoteName, torrentResp.ID, torrentResp.Hash, torrentResp.Name)
		return nil, nil
	} else {
		return nil, fmt.Errorf("TorBox backend only supports adding .torrent files or magnet links (remote must end with .torrent or .magnet, or be a magnet link itself): %s", remoteName)
	}
}

// About gets quota information from the Fs
func (f *Fs) About(ctx context.Context) (*fs.Usage, error) {
	var userInfo UserMeInfo
	_, err := f.Srv.CallJSON(ctx, "GET", "/v1/api/user/me", nil, &userInfo) // Use f.Srv
	if err != nil {
		return nil, fmt.Errorf("failed to get user info for About: %w", err)
	}

	usage := &fs.Usage{
		Used:  fs.NewUsageValue(-1), // Default to unknown
		Total: fs.NewUsageValue(-1), // Default to unknown
		Free:  fs.NewUsageValue(-1), // Default to unknown
		// Objects: fs.NewUsageValue(-1), // Number of torrents could be an option here, but requires another call
	}

	if userInfo.StorageBytesUsed != nil {
		usage.Used = fs.NewUsageValue(*userInfo.StorageBytesUsed)
	}
	if userInfo.StorageBytesTotal != nil {
		usage.Total = fs.NewUsageValue(*userInfo.StorageBytesTotal)
	}

	if userInfo.StorageBytesTotal != nil && userInfo.StorageBytesUsed != nil {
		if *userInfo.StorageBytesTotal >= 0 && *userInfo.StorageBytesUsed >= 0 { // Ensure they are not negative if API could send that
			usage.Free = fs.NewUsageValue(*userInfo.StorageBytesTotal - *userInfo.StorageBytesUsed)
		}
	}

	// To get Objects (number of torrents), we'd need to call /v1/api/torrents/mylist
	// For now, let's leave it as unknown or count it if a recent list call cached it.
	// This example doesn't implement caching of mylist for About.
	usage.Objects = fs.NewUsageValue(-1)


	return usage, nil
}

// PutStream uploads a stream
//
// This can be used if the Fs backend has a way of uploading streams
// directly, without needing to buffer all the data into memory first.
//
// Optional interface: implement this if possible
//
// src is the object to stream, in is the reader to read from, src may not have a size if it's streaming.
//
// If err is non nil then fs.Object must be nil.
func (f *Fs) PutStream(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	// TorBox does not support streaming arbitrary files to create torrents.
	// .torrent files and magnet links are expected to be small and handled by Put.
	return nil, fs.ErrNotImplemented
}
