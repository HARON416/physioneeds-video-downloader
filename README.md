# Physioneeds Video Downloader

A Go application for organizing, processing, and downloading video lessons from a Physioneeds course page. Use it only with courses and materials you are authorized to access and download, and in accordance with the platform's terms.

## What it does

- Opens the course URL in a dedicated Chrome profile.
- Waits for you to sign in when a login page is detected.
- Discovers video lessons in the order they appear in the course page's DOM and keeps their module names.
- **Does not download PDFs or other non-video lesson types.**
- Captures lesson playlist requests, retries missing captures, and reports lessons whose requests were not captured.
- Saves output under `downloads/<course title>/<module>/<lesson title>.mp4`.
- Numbers duplicate lesson titles within the same module (`Lesson 1.mp4`, `Lesson 2.mp4`, etc.) to avoid overwriting them, and removes the old unsuffixed file only after all numbered replacements exist.
- Starts traversal from the first video lesson on each run. Non-empty MP4 files already at the expected output path are skipped.

## Requirements

- Go 1.25.6 or newer.
- Google Chrome or Chromium.
- `ffmpeg` on `PATH` for video processing.
- Authorized access to the course in the browser opened by the app.

## Run

From the project directory:

```sh
go run .
```

Enter the course URL when prompted. If the browser shows a login page, sign in in the Chrome window launched by the app, then press Enter in the terminal running it.

The app uses `browser_profile/` to retain that browser session between runs. Keep this profile private. The app may also request provider-specific configuration; obtain any required values through an authorized provider channel.

After login, the app uses `SPAYEE_APK_ID` from the environment if set; otherwise, it looks for a unique hexadecimal `apkId` value on the logged-in page and prompts if none or multiple distinct values are found. The detected value is used for media-key processing and is not printed during a normal run.

## Output and incomplete captures

Module folders are based on the course page's displayed title and lesson sections. Existing non-empty MP4 files are skipped, but each run still begins traversing from the first video lesson in DOM order.

Captured playlist references are written to `downloads.go`. Treat this file as local session data and do not share it. A lesson for which no playlist request is captured after retries is reported in the terminal; an incomplete capture should not be treated as a complete course run.

## Test

```sh
go test ./...
go vet ./...
```

## Help

For help, contact Haron Kibetrutoh by [email](mailto:haronkibetrutoh@gmail.com) or [WhatsApp](https://wa.me/254718448461).
# physioneeds-video-downloader
