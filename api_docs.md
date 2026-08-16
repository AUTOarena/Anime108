# Anime108 Scraper Backend API Documentation

This document describes the JSON API endpoints exposed by the Go server (`main.go`).

Default Base URL: `http://localhost:5000`

---

## 1. Search Anime
Searches the site for matching anime and returns a list of results.

- **Route**: `GET /search`
- **Query Parameters**:
  - `keyword` or `q` (string): The search query keyword (e.g. `mushen`).
- **Response** (200 OK):
  ```json
  [
    {
      "title": "Mushen Ji (Tales of Herding Gods) ตำนานเทพกู้จักรวาล",
      "url": "https://www.anime108.com/mushen-ji/",
      "image": "https://www.anime108.com/wp-content/uploads/2024/10/Mushen-Ji.jpg",
      "episodes_info": "ตอนที่ 1-85"
    }
  ]
  ```

---

## 2. Parse Show Page
Extracts show metadata, post ID, and episode lists (dubbed/subbed).

- **Route**: `POST /api/parse`
- **Content-Type**: `application/json`
- **Request Body**:
  ```json
  {
    "url": "https://www.anime108.com/mushen-ji/"
  }
  ```
- **Response** (200 OK):
  ```json
  {
    "title": "Mushen Ji (Tales of Herding Gods) ตำนานเทพกู้จักรวาล (พากย์ไทย ซับไทย)",
    "post_id": 19430,
    "current_episode": null,
    "episodes": {
      "Thai": [
        {
          "title": "ตอนที่ 1",
          "url": "https://www.anime108.com/mushen-ji-ep-1/"
        }
      ],
      "Sound Track": [
        {
          "title": "ตอนที่ 1",
          "url": "https://www.anime108.com/mushen-ji-ep-1/"
        }
      ]
    }
  }
  ```

---

## 3. Resolve Player Stream URL
Resolves the raw streaming player iframe URL for a specific episode. Useful for direct browser playback (streaming) without saving files to host disk.

- **Route**: `POST /api/player-url`
- **Content-Type**: `application/json`
- **Request Body**:
  ```json
  {
    "url": "https://www.anime108.com/mushen-ji-ep-2/",
    "lang": "Sound Track"
  }
  ```
  *(Note: `lang` can be `"Sound Track"` for Subbed, or `"Thai"` for Dubbed. Default is `"Sound Track"`)*
- **Response** (200 OK):
  ```json
  {
    "iframe_url": "https://main.108player.com/index_th.php?id=dc293979261c3a1b852d6e2e"
  }
  ```

---

## 4. Trigger Video Download (Background Task)
Spawns an asynchronous background thread to resolve, download, remux, and assemble segments into an MP4 file.

- **Route**: `POST /api/download`
- **Content-Type**: `application/json`
- **Request Body**:
  ```json
  {
    "url": "https://www.anime108.com/mushen-ji-ep-2/",
    "lang": "Sound Track"
  }
  ```
- **Response** (200 OK):
  ```json
  {
    "task_id": "893c52a8-12d4-42b7-8ca1-689cdbe5e43a"
  }
  ```

---

## 5. Query Download Task Progress
Polls the active download state, chunk counts, percentage progress, and status logs.

- **Route**: `GET /api/progress/<task_id>`
- **Response** (200 OK):
  ```json
  {
    "status": "downloading",
    "progress": 45,
    "total": 173,
    "percentage": 26,
    "message": "Downloading chunk 45/173",
    "title": "Mushen Ji (Tales of Herding Gods) - Ep 2",
    "lang": "Sound Track",
    "url": "https://www.anime108.com/mushen-ji-ep-2/"
  }
  ```
  *(Possible values for `status`: `"idle"`, `"downloading"`, `"merging"`, `"completed"`, `"failed"`)*

---

## 6. List Downloaded Files
Returns files currently inside the local `downloads/` directory.

- **Route**: `GET /api/downloads`
- **Response** (200 OK):
  ```json
  {
    "downloads": [
      {
        "filename": "Mushen Ji (Tales of Herding Gods) - Ep 2 (Sound Track).mp4",
        "size": "245.2 MB",
        "path": "C:\\Users\\USER\\...\\downloads\\Mushen Ji (Tales of Herding Gods) - Ep 2 (Sound Track).mp4"
      }
    ]
  }
  ```
