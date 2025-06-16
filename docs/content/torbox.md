---
title: "TorBox"
description: "Rclone backend for TorBox file storage."
version: "vX.Y" # Rclone version - adjust when released
status: "beta"  # Current status
supports_server_side: ["delete"] # "delete" for torrents (directories)
options:
- name: "api_token"
  description: |
    Your TorBox API Token. This is required to authenticate with the TorBox API.
    See below for instructions on how to obtain this token.
  required: true
  example: "your_actual_api_token_value_here"
  obscure: true # Mark as sensitive
---

# TorBox

[TorBox](https://torbox.app/) is a service that allows you to download torrents and other files from the internet to your private cloud storage space. This rclone backend enables you to manage and access the files stored within your TorBox account, primarily focusing on listing torrents and the files they contain, downloading these files, and managing your torrents by adding or deleting them.

## Configuration

To configure a TorBox remote for rclone, run `rclone config` and follow the interactive prompts. You will need your **API Token**.

### Obtaining your API Token

The TorBox API uses a bearer token for authentication. Obtaining this `api_token` is a multi-step process:

1.  **Log in to your TorBox account:** Open your web browser and log in at [https://torbox.app/](https://torbox.app/).
2.  **Access Developer Tools:** Once logged in, open your browser's developer tools. This is typically done by pressing `F12`.
3.  **Find your Session Token:**
    *   Navigate to the 'Application' (in Chrome/Edge) or 'Storage' (in Firefox) tab within the developer tools.
    *   Look for 'Cookies' in the sidebar and select the cookie(s) associated with `torbox.app` or `www.torbox.app`.
    *   Find a cookie named `session_token` (or a similar name like `connect.sid`). Copy its value. This is your temporary session token.
4.  **Exchange Session Token for API Token:** Use a command-line tool like `curl` to exchange your `session_token` for a long-lived `api_token`. Replace `YOUR_SESSION_TOKEN_HERE` with the actual value you copied:
    ```bash
    curl -X POST -H "Content-Type: application/json" \
         -d '{"session_token": "YOUR_SESSION_TOKEN_HERE"}' \
         https://api.torbox.app/v1/api/user/refreshtoken
    ```
5.  **Extract API Token:** The response from the `curl` command will be a JSON object. Look for a field named `token` (or `api_token`). This is your `api_token` for rclone.
    Example response: `{"token":"your_actual_api_token_value_here", "other_field": "..."}`
6.  **Use in Rclone Config:** Enter this `api_token` when prompted during `rclone config`.

**Example rclone.conf entry:**
```ini
[torbox_remote]
type = torbox
api_token = your_actual_api_token_value_here
```

## Path Handling and Object Model

The TorBox backend models your torrents and their contents as follows:

*   **Torrents as Directories:** Each torrent in your TorBox account is represented as a "directory" in rclone.
*   **Directory Naming:** The names of these torrent "directories" are currently formed using the **torrent's hash**. For example, if a torrent has the hash `a1b2c3d4...`, it will appear as a directory named `a1b2c3d4...`.
    *   You can list these torrent directories using `rclone lsd torbox_remote:`.
*   **Files within Torrents:** Files contained within a torrent are listed inside their corresponding torrent hash "directory".
    *   For example, to list files in the torrent with hash `a1b2c3d4...`, you would use `rclone ls torbox_remote:/a1b2c3d4.../`.
*   **File Paths:** The full remote path to a file within a torrent typically looks like `TORRENT_HASH/path/to/file_in_torrent.ext`. The `path/to/file_in_torrent.ext` part reflects the file's path as it is structured inside the torrent.

## Core Operations

*   **Listing Torrents:**
    *   `rclone lsd torbox_remote:` - Lists all active torrents (as directories named by their hash).
*   **Listing Files in a Torrent:**
    *   `rclone ls torbox_remote:/TORRENT_HASH/` - Lists files and subdirectories within the specified torrent.
    *   `rclone lsjson torbox_remote:/TORRENT_HASH/` - Provides detailed JSON output for files.
*   **Downloading Files:**
    *   `rclone copy torbox_remote:/TORRENT_HASH/path/to/file.txt /local/destination/` - Downloads a specific file.
*   **Adding New Torrents:**
    *   This backend allows you to add new torrents to your TorBox account using `.torrent` files or magnet links. This operation registers the torrent with TorBox for downloading; it does not upload the torrent's actual data content via rclone.
    *   **Using a `.torrent` file:**
        ```bash
        rclone copy mylocal.torrent torbox_remote:mylocal.torrent
        ```
        The name given on the remote side (e.g., `mylocal.torrent`) is primarily for rclone's operation tracking. TorBox will identify the torrent based on its metadata content, and the name appearing in listings will be derived from that or the torrent hash.
    *   **Using a magnet link:**
        1.  Save your complete magnet URI into a plain text file (e.g., `mymagnet.txt`).
        2.  Copy this file to the remote, ensuring the remote filename ends with `.magnet`:
            ```bash
            rclone copy mymagnet.txt torbox_remote:desired_name.magnet
            ```
            Rclone reads the content of the source file (`mymagnet.txt`) and sends it as the magnet link to TorBox. The `desired_name.magnet` is for rclone's path handling.
*   **Deleting Torrents:**
    *   To delete an entire torrent and all its files from your TorBox account:
        ```bash
        rclone purge torbox_remote:/TORRENT_HASH
        ```
        or
        ```bash
        rclone rmdir torbox_remote:/TORRENT_HASH
        ```
    *   **Important:** Deleting individual files within a torrent (e.g., `rclone deletefile remote:TORRENT_HASH/file.txt`) is **not supported** due to limitations in the TorBox API.

## Limitations

*   **No Individual File Deletion:** As stated above, individual files within an existing torrent cannot be deleted via rclone due to API limitations. You must delete the entire torrent and re-add it if you wish to exclude certain files.
*   **Uploads Limited to Torrents/Magnets:** The `rclone copy` command, when copying *to* the TorBox remote, is only for adding new `.torrent` files or magnet links. It does not support uploading arbitrary files (e.g., a ZIP archive or a video file) to be converted into new torrents or stored directly.
*   **No Server-Side Copy/Move:** Server-side copy (`rclone copy remote:file1 remote:file2`) and server-side move (`rclone move remote:file1 remote:file2`) operations for files within or between torrents are not supported. Torrent "directories" also cannot be copied or moved server-side.
*   **Modification Times (`ModTime`):** The modification time for files listed within a torrent is derived from the **torrent's creation time** on TorBox. Individual files do not expose their original modification times via the API. The precision of this reported time is per second.
*   **File Hashes:** The TorBox API does not provide checksum hashes (like MD5, SHA1) for individual files within torrents. Therefore, `rclone sha1sum`, `rclone md5sum`, etc., will show "Unsupported" for files on this backend.
*   **Storage Quota Information (`rclone about`):** The information displayed by `rclone about torbox_remote:` regarding total and used storage space is based on what the TorBox API (`/v1/api/user/me`) provides. If certain fields (like total, free, or object count) are not available or are returned as null/zero by the API, they may appear as -1 (unknown) or 0 in the `rclone about` output.
*   **Directory Modification Times:** The modification time for torrent "directories" (listed by `rclone lsd`) is the creation time of the torrent.

---
Standard options like `type` and `api_token` are described above. There are currently no other backend-specific advanced options for TorBox. For general rclone options, please refer to the main rclone documentation.
