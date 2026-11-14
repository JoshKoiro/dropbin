# dropbin

Minimal self-hosted drop box for sending video files to your media server. Drag and drop in a browser and files land in your library folders. Single Go binary, ~10 MB Docker image, configured entirely from docker-compose.

- One Go binary on a `scratch` image (~8 MB image, a few MB of RAM idle)
- Uploads stream straight to disk, so no RAM or temp-space blowup on 50 GB files
- Files are written as hidden `.Name.mkv.part` and renamed when complete, so your media server never scans half a file
- Optional server-side "fetch from link" for direct file URLs
- Everything is configured in `docker-compose.yml`

## Setup

1. Put this folder on your server.
2. Edit `docker-compose.yml`:
   - **volumes**: mount each library folder under `/library/<Name>`, using the **same host path** your media server container already uses. Each one becomes a destination button.
     ```yaml
     volumes:
       - /srv/media/movies:/library/Movies
       - "/srv/media/tv:/library/TV Shows"
     ```
     Or mount one parent folder (`/srv/media:/library`) and its subfolders show up automatically.
   - **user**: set it to the UID:GID that owns those folders (run `ls -ln /srv/media`) or that your media server runs as. Common values are `1000:1000` for linuxserver.io images and Jellyfin's `PUID/PGID`.
3. Start it:
   ```bash
   docker compose up -d --build
   ```
4. Open `http://<server-ip>:8080`.

## Settings (environment)

| Variable | Default | What it does |
|---|---|---|
| `TITLE` | `dropbin` | Page title |
| `PASSWORD` | *(empty)* | Turns on a browser login prompt. Any username works. |
| `ALLOWED_EXTENSIONS` | common video + subtitle types | Comma list, or `*` for anything |
| `MAX_UPLOAD_GB` | `0` | Per-file cap, `0` = unlimited |
| `OVERWRITE` | `false` | `false` saves duplicates as `Name (1).mkv` |
| `ENABLE_URL_FETCH` | `true` | Shows the "From a link" tab |
| `PORT` | `8080` | Port inside the container |
| `LIBRARY_ROOT` | `/library` | Folder inside the container that holds your destinations |

## Using it

1. Pick a destination (it remembers the last one).
2. Optionally type a subfolder like `Severance/Season 01`. Existing folders autocomplete, and new ones are created for you.
3. Drop files anywhere on the page. They upload one at a time, with speed and time left.

**From a link** only works with *direct* file URLs (the link ends in `.mp4`, `.mkv`, etc. or downloads a file). It can't pull from YouTube-style pages; for those, download the file locally first and drop it in.

## Notes

- If you put this behind a reverse proxy, raise its body-size limit (nginx: `client_max_body_size 0;`) and turn off request buffering (`proxy_request_buffering off;`), or big uploads will fail.
- If you leave `PASSWORD` empty, keep the app on your LAN or VPN only.
- Most media servers pick up new files on their own. If yours doesn't, run a library scan.

## Files

```
main.go          server (Go standard library only)
web/index.html   the whole UI, embedded into the binary
Dockerfile       multi-stage build → scratch image
```
