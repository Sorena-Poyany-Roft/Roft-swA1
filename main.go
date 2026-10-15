
package main

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const maxFileSize = 1 << 20 // 1 MiB

type FileInfo struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

func main() {
	token := os.Getenv("ROFT_ACCESS_TOKEN")
	if token == "" {
		log.Fatal("ROFT_ACCESS_TOKEN is required")
	}

	root := os.Getenv("ROFT_DATA_DIR")
	if root == "" {
		root = "./roft-files"
	}

	if err := os.MkdirAll(root, 0750); err != nil {
		log.Fatal(err)
	}

	root, err := filepath.Abs(root)
	if err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" || r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, panelHTML)
	})

	mux.Handle("/api/files", authorize(token, http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
				return
			}

			files := make([]FileInfo, 0)
			err := filepath.WalkDir(root, func(
				name string, entry os.DirEntry, walkErr error,
			) error {
				if walkErr != nil {
					return walkErr
				}
				if entry.Type()&os.ModeSymlink != 0 {
					if entry.IsDir() {
						return filepath.SkipDir
					}
					return nil
				}
				if entry.IsDir() {
					return nil
				}
				if !entry.Type().IsRegular() {
					return nil
				}

				rel, err := filepath.Rel(root, name)
				if err != nil {
					return err
				}
				info, err := entry.Info()
				if err != nil {
					return err
				}

				files = append(files, FileInfo{
					Path: filepath.ToSlash(rel),
					Size: info.Size(),
				})
				if len(files) >= 2000 {
					return errors.New("file listing limit reached")
				}
				return nil
			})

			if err != nil && err.Error() != "file listing limit reached" {
				http.Error(w, "Could not list files", http.StatusInternalServerError)
				return
			}

			sort.Slice(files, func(i, j int) bool {
				return files[i].Path < files[j].Path
			})
			sendJSON(w, files)
		},
	)))

	mux.Handle("/api/file", authorize(token, http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			rel := r.URL.Query().Get("path")
			if rel == "" {
				http.Error(w, "File path is required", http.StatusBadRequest)
				return
			}

			switch r.Method {
			case http.MethodGet:
				name, err := safePath(root, rel, false)
				if err != nil {
					http.Error(w, "Invalid file path", http.StatusBadRequest)
					return
				}

				info, err := os.Stat(name)
				if err != nil || !info.Mode().IsRegular() {
					http.Error(w, "File not found", http.StatusNotFound)
					return
				}
				if info.Size() > maxFileSize {
					http.Error(w, "File is too large to edit", http.StatusRequestEntityTooLarge)
					return
				}

				data, err := os.ReadFile(name)
				if err != nil {
					http.Error(w, "Could not read file", http.StatusInternalServerError)
					return
				}
				w.Header().Set("Content-Type", "text/plain; charset=utf-8")
				w.Write(data)

			case http.MethodPut:
				r.Body = http.MaxBytesReader(w, r.Body, maxFileSize)
				defer r.Body.Close()

				data, err := io.ReadAll(r.Body)
				if err != nil {
					http.Error(w, "Invalid or oversized file content", http.StatusBadRequest)
					return
				}

				name, err := safePath(root, rel, true)
				if err != nil {
					http.Error(w, "Invalid file path", http.StatusBadRequest)
					return
				}

				if info, err := os.Stat(name); err == nil && !info.Mode().IsRegular() {
					http.Error(w, "Target is not a regular file", http.StatusBadRequest)
					return
				}

				if err := os.WriteFile(name, data, 0640); err != nil {
					http.Error(w, "Could not save file", http.StatusInternalServerError)
					return
				}

				sendJSON(w, map[string]string{"status": "saved"})

			default:
				http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			}
		},
	)))

	port := os.Getenv("PORT")
	if port == "" {
		port = "10000"
	}

	server := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Printf("Roft file panel listening on port %s", port)
	log.Fatal(server.ListenAndServe())
}

func authorize(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const prefix = "Bearer "
		auth := r.Header.Get("Authorization")

		if !strings.HasPrefix(auth, prefix) ||
			!constantTimeEqual(strings.TrimPrefix(auth, prefix), token) {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func constantTimeEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := 0; i < len(a); i++ {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

func safePath(root, rel string, createParents bool) (string, error) {
	if rel == "" || strings.Contains(rel, "\\") || strings.HasPrefix(rel, "/") {
		return "", errors.New("invalid path")
	}

	parts := strings.Split(rel, "/")
	current := root

	for i, part := range parts {
		if part == "" || part == "." || part == ".." {
			return "", errors.New("invalid path component")
		}

		current = filepath.Join(current, part)
		info, err := os.Lstat(current)

		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				return "", errors.New("symbolic links are not allowed")
			}
			if i < len(parts)-1 && !info.IsDir() {
				return "", errors.New("parent is not a directory")
			}
		} else if os.IsNotExist(err) {
			if i < len(parts)-1 && createParents {
				if err := os.Mkdir(current, 0750); err != nil {
					return "", err
				}
			}
		} else {
			return "", err
		}
	}

	relCheck, err := filepath.Rel(root, current)
	if err != nil || relCheck == ".." ||
		strings.HasPrefix(relCheck, ".."+string(os.PathSeparator)) {
		return "", errors.New("path escapes project directory")
	}

	return current, nil
}

func sendJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(value)
}

const panelHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Roft File Panel</title>
<style>
body{margin:0;background:#11131a;color:#cdd6f4;font:15px system-ui}
header{padding:18px;background:#181825;border-bottom:1px solid #313244}
main{display:grid;grid-template-columns:minmax(200px,30%) 1fr;gap:16px;padding:16px}
section{background:#181825;border:1px solid #313244;border-radius:10px;padding:14px;min-width:0}
input,textarea,button{box-sizing:border-box;width:100%;padding:10px;margin:5px 0;background:#11131a;color:#cdd6f4;border:1px solid #45475a;border-radius:6px}
textarea{height:55vh;resize:vertical;font:14px monospace;white-space:pre;tab-size:4}
button{cursor:pointer;background:#313244}
button:hover{border-color:#89b4fa}
.file{display:block;text-align:left;overflow-wrap:anywhere}
small,#status{color:#a6adc8}
@media(max-width:700px){main{grid-template-columns:1fr}}
</style>
</head>
<body>
<header><strong>ROFT</strong> / File Panel</header>
<main>
<section>
<h3>Connection</h3>
<input id="token" type="password" placeholder="Access token" autocomplete="off">
<button id="connect">Connect</button>
<button id="disconnect">Disconnect</button>
<p><small>The token stays in memory and is cleared when this page reloads.</small></p>
<hr>
<h3>Files</h3>
<button id="refresh">Refresh list</button>
<div id="files"></div>
<hr>
<h3>New file</h3>
<input id="newpath" placeholder="src/main.go">
<button id="create">Create / open file</button>
</section>
<section>
<h3 id="filename">No file selected</h3>
<textarea id="editor" spellcheck="false" placeholder="Choose a file or enter a new file path." disabled></textarea>
<button id="save" disabled>Save file</button>
<p id="status" role="status">Not connected</p>
</section>
</main>
<script>
let token = "";
let currentPath = "";

const $ = id => document.getElementById(id);
function status(message) { $("status").textContent = message; }

async function api(url, options = {}) {
  const headers = new Headers(options.headers || {});
  headers.set("Authorization", "Bearer " + token);
  const response = await fetch(url, {...options, headers});
  if (!response.ok) throw new Error(await response.text());
  return response;
}

async function refreshFiles() {
  const response = await api("/api/files");
  const files = await response.json();
  const list = $("files");
  list.replaceChildren();
  for (const file of files) {
    const button = document.createElement("button");
    button.className = "file";
    button.textContent = file.path;
    button.onclick = () => openFile(file.path);
    list.appendChild(button);
  }
  status("Loaded " + files.length + " file(s)");
}

async function openFile(path) {
  try {
    const response = await api("/api/file?path=" + encodeURIComponent(path));
    $("editor").value = await response.text();
    currentPath = path;
    $("filename").textContent = path;
    $("newpath").value = path;
    $("editor").disabled = false;
    $("save").disabled = false;
    status("Opened " + path);
  } catch (error) { status(error.message); }
}

$("connect").onclick = async () => {
  token = $("token").value.trim();
  try {
    await refreshFiles();
  } catch (error) {
    token = "";
    status("Connection failed: " + error.message);
  }
};

$("disconnect").onclick = () => {
  token = "";
  currentPath = "";
  $("token").value = "";
  $("editor").value = "";
  $("editor").disabled = true;
  $("save").disabled = true;
  $("files").replaceChildren();
  $("filename").textContent = "No file selected";
  status("Disconnected");
};

$("refresh").onclick = async () => {
  try { await refreshFiles(); }
  catch (error) { status(error.message); }
};

$("create").onclick = async () => {
  const path = $("newpath").value.trim();
  if (!path) { status("Enter a file path first"); return; }
  if (currentPath === path) { status("This file is already open"); return; }
  try {
    const response = await api("/api/file?path=" + encodeURIComponent(path));
    $("editor").value = await response.text();
  } catch (error) {
    if (error.message.includes("404") || error.message.includes("File not found")) {
      $("editor").value = "";
    } else {
      status(error.message);
      return;
    }
  }
  currentPath = path;
  $("filename").textContent = path + " (new or existing)";
  $("editor").disabled = false;
  $("save").disabled = false;
  status("Ready to edit " + path);
};

$("save").onclick = async () => {
  if (!currentPath) { status("Choose a file first"); return; }
  try {
    await api("/api/file?path=" + encodeURIComponent(currentPath), {
      method: "PUT",
      headers: {"Content-Type": "text/plain; charset=utf-8"},
      body: $("editor").value
    });
    status("Saved " + currentPath);
    await refreshFiles();
  } catch (error) { status("Save failed: " + error.message); }
};
</script>
</body>
</html>`
