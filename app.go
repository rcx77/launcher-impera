package main

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/sirupsen/logrus"
	"github.com/spf13/viper"
)

// ManifestFile matches one entry in the "files" array produced by
// KrayAccOpenTibia's manifest generator (cmd/manifest -create_manifest):
//
//	{"path": "mods/game_helper/game_helper.lua", "size": 1234, "sha256": "..."}
//
// There is no separate "packed" vs "unpacked" hash here: KrayAcc serves the
// client files as plain, uncompressed files, so a single hash/size pair is
// enough to know whether a local file is missing or out of date.
type ManifestFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	Sha256 string `json:"sha256"`
	// URL is optional and normally empty. When present it overrides the
	// default download location (ManifestBaseURL + "/" + Path) - kept only
	// for forward-compatibility with KrayAcc's schema, which declares it.
	URL string `json:"url,omitempty"`
}

// Manifest matches the JSON served by KrayAccOpenTibia at GET /client/manifest
// (see src/models/laucher.go and cmd/manifest/main.go in that project).
type Manifest struct {
	App     string         `json:"app"`
	Version string         `json:"version"`
	BaseURL string         `json:"base_url"`
	Files   []ManifestFile `json:"files"`
}

type App struct {
	ctx context.Context

	logger *logrus.Logger

	// manifestURL is the full URL of KrayAcc's manifest endpoint, e.g.
	// "http://YOUR_SERVER:PORT/client/manifest".
	manifestURL string
	// filesBaseURL is where the actual client files are downloaded from. It
	// is NOT read from config - it comes from the manifest's own "base_url"
	// field (KrayAcc already points this at "/launcher_client"), so the
	// launcher always follows whatever the server says.
	filesBaseURL string
	appName      string

	manifest Manifest

	totalBytes      int64
	totalFiles      int64
	downloadedBytes int64
	downloadedFiles int64

	parallel int

	activeDownloads map[string]struct{}
	mutex           sync.Mutex

	queue  chan ManifestFile
	cancel chan struct{}
}

func NewApp(logger *logrus.Logger, manifestURL string, appName string, parallel int) *App {
	return &App{
		logger:          logger,
		manifestURL:     manifestURL,
		queue:           make(chan ManifestFile, 16),
		cancel:          make(chan struct{}),
		activeDownloads: make(map[string]struct{}),
		parallel:        parallel,
		appName:         appName,
	}
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
}

func (a *App) OpenClientLocation() {
	fmt.Println("Opening client location")
	if runtime.GOOS == "darwin" {
		exec.Command("open", a.appDirectory()).Start()
	} else if runtime.GOOS == "windows" {
		exec.Command("explorer", a.appDirectory()).Start()
	} else if runtime.GOOS == "linux" {
		exec.Command("xdg-open", a.appDirectory()).Start()
	}
}

func (a *App) Exit() {
	os.Exit(0)
}

// refreshManifests fetches the manifest directly from KrayAcc's server
// (it's generated dynamically by the Go backend, not a static file, so we
// read it straight from the HTTP response instead of saving it to disk
// first like the old CIP-style client.json/assets.json did).
func (a *App) refreshManifests() {
	resp, err := http.Get(a.manifestURL)
	if err != nil {
		a.logger.Errorf("Error fetching manifest from %s: %v", a.manifestURL, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		a.logger.Errorf("Error fetching manifest from %s: HTTP %d", a.manifestURL, resp.StatusCode)
		return
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		a.logger.Errorf("Error reading manifest body: %v", err)
		return
	}

	var m Manifest
	if err := json.Unmarshal(body, &m); err != nil {
		a.logger.Errorf("Error parsing manifest JSON: %v", err)
		return
	}

	a.manifest = m
	if m.BaseURL != "" {
		a.filesBaseURL = m.BaseURL
	}
}

func (a *App) Version() string {
	a.refreshManifests()
	return a.manifest.Version
}

func (a *App) DownloadPercent() float64 {
	if a.totalBytes == 0 {
		return 0
	}
	percent := float64(a.downloadedBytes) / float64(a.totalBytes) * 100
	a.logger.Infof("Downloaded %d/%d files |  %d/%d bytes (%.2f%%)", a.downloadedFiles, a.totalFiles, a.downloadedBytes, a.totalBytes, percent)
	return percent
}

func (a *App) TotalFiles() int64 {
	return a.totalFiles
}

func (a *App) TotalBytes() int64 {
	return a.totalBytes
}

func (a *App) DownloadedFiles() int64 {
	return a.downloadedFiles
}

func (a *App) DownloadedBytes() int64 {
	return a.downloadedBytes
}

func (a *App) ToggleLocal(value bool) {
	a.logger.Infof("Setting enableLocal to %v", value)
	viper.Set("enableLocal", value)
	a.saveConfig()
}

func (a *App) saveConfig() {
	if err := viper.WriteConfigAs(filepath.Join(configDirectory(a.appName), "config.toml")); err != nil {
		a.logger.Errorf("Error writing config: %v", err)
	}
}

func (a *App) LocalEnabled() bool {
	return viper.GetBool("enableLocal")
}

func (a *App) OS() string {
	os := runtime.GOOS
	if os == "darwin" {
		return "mac"
	}
	return os
}

func (a *App) ActiveDownload() string {
	a.mutex.Lock()
	defer a.mutex.Unlock()
	for url := range a.activeDownloads {
		return url
	}
	return ""
}

func (a *App) Update() {
	files, err := a.filesToUpdate()
	if err != nil {
		a.logger.Errorf("Error checking for updates: %v", err)
	}

	a.totalFiles = int64(len(files))
	a.totalBytes = 0
	a.downloadedFiles = 0
	a.downloadedBytes = 0
	for _, file := range files {
		a.totalBytes += file.Size
	}

	for i := 0; i < a.parallel; i++ {
		go func() {
			for {
				select {
				case <-a.cancel:
					return
				case <-a.ctx.Done():
					return
				case file := <-a.queue:
					url := a.fileDownloadURL(file)
					a.mutex.Lock()
					a.activeDownloads[file.Path] = struct{}{}
					a.mutex.Unlock()
					err := a.downloadFile(url, file.Path, true)
					a.mutex.Lock()
					delete(a.activeDownloads, file.Path)
					a.mutex.Unlock()
					if err != nil {
						a.logger.Errorf("Error downloading %s: %v", url, err)
						return
					}
					a.logger.Debugf("Downloaded %s", url)
				}
			}
		}()
	}

	for _, file := range files {
		a.queue <- file
	}
}

var mapKinds = map[int]string{
	0: "https://tibiamaps.github.io/tibia-map-data/minimap-with-markers.zip",
	1: "https://tibiamaps.github.io/tibia-map-data/minimap-without-markers.zip",
	2: "https://tibiamaps.github.io/tibia-map-data/minimap-with-grid-overlay-and-markers.zip",
	3: "https://tibiamaps.io/downloads/minimap-with-grid-overlay-without-markers",
	4: "https://tibiamaps.github.io/tibia-map-data/minimap-with-grid-overlay-and-poi-markers.zip",
}

var mapLocations = map[string]string{
	"mac":     "Contents/Resources/minimap",
	"windows": "minimap",
	"linux":   "minimap",
}

func (a *App) DownloadMaps(kind int) {
	a.totalBytes = 0
	a.downloadedBytes = 0
	a.totalFiles = 1
	a.downloadedFiles = 0
	a.logger.Infof("Downloading %s", mapKinds[kind])
	err := a.downloadZip(mapKinds[kind], mapLocations[a.OS()], true)
	if err != nil {
		a.logger.Errorf("Error downloading %s: %v", mapKinds[kind], err)
		return
	}
}

func (a *App) NeedsUpdate() bool {
	a.refreshManifests()
	files, err := a.filesToUpdate()
	if err != nil {
		a.logger.Errorf("Error checking for updates: %v", err)
		return false
	}
	return len(files) > 0
}

func (a *App) appDirectory() string {
	configDir, err := os.UserConfigDir()
	if err != nil {
		a.logger.Errorf("Error getting config directory: %v", err)
		return ""
	}
	appName := a.appName
	if a.OS() == "mac" {
		appName = a.appName + ".app"
	}
	return filepath.Join(configDir, appName)
}

// fileDownloadURL resolves where a given manifest entry should be downloaded
// from. KrayAcc's FileEntry.URL is normally empty (omitempty), meaning
// "download from filesBaseURL + '/' + Path" - that base URL itself comes
// from the manifest's own "base_url" field (see refreshManifests), which
// KrayAcc already points at its "/launcher_client" static file server.
func (a *App) fileDownloadURL(f ManifestFile) string {
	if f.URL != "" {
		return f.URL
	}
	base := strings.TrimSuffix(a.filesBaseURL, "/")
	path := strings.TrimPrefix(f.Path, "/")
	return base + "/" + path
}

func (a *App) filesToUpdate() ([]ManifestFile, error) {
	var files []ManifestFile
	filesTocheck := a.manifest.Files

	mutex := sync.Mutex{}
	wg := sync.WaitGroup{}
	wg.Add(len(filesTocheck))

	for _, file := range filesTocheck {
		go func(file ManifestFile) {
			defer wg.Done()

			localFilePath := filepath.Join(a.appDirectory(), file.Path)
			if !fileExists(localFilePath) {
				a.logger.Infof("File %s does not exist", localFilePath)
				mutex.Lock()
				files = append(files, file)
				mutex.Unlock()
			} else {
				localHash, err := sha256Sum(localFilePath)
				if err != nil {
					a.logger.Errorf("Error reading local file: %s\n", err)
					return
				}

				if localHash != file.Sha256 {
					a.logger.Infof("File %s has changed (local: %s, remote: %s)", localFilePath, string(localHash), file.Sha256)
					mutex.Lock()
					files = append(files, file)
					mutex.Unlock()
				}
			}
		}(file)
	}

	wg.Wait()

	return files, nil
}

func fileExists(path string) bool {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return false
	}
	return true
}

func (a *App) downloadZip(url, dst string, progress bool) error {
	dst = filepath.Join(a.appDirectory(), dst)
	err := os.MkdirAll(filepath.Dir(dst), 0755)
	if err != nil {
		return err
	}

	out, err := os.Create(filepath.Join(os.TempDir(), filepath.Base(dst)))
	if err != nil {
		return err
	}
	defer out.Close()

	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return err
	}

	a.totalBytes = resp.ContentLength

	var reader io.Reader = resp.Body
	if progress {
		reader = io.TeeReader(reader, a)
	}
	_, err = io.Copy(out, reader)
	if err != nil {
		return err
	}
	out.Close()

	err = unzip(out.Name(), filepath.Dir(dst))
	if err != nil {
		return err
	}

	a.downloadedFiles++

	return nil
}

func unzip(src, dst string) error {
	r, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer r.Close()

	for _, f := range r.File {
		if f.FileInfo().IsDir() {
			err := os.MkdirAll(filepath.Join(dst, f.Name), 0755)
			if err != nil {
				return err
			}
			continue
		}

		err := os.MkdirAll(filepath.Join(dst, filepath.Dir(f.Name)), 0755)
		if err != nil {
			return err
		}

		rc, err := f.Open()
		if err != nil {
			return err
		}

		out, err := os.Create(filepath.Join(dst, f.Name))
		if err != nil {
			return err
		}

		_, err = io.Copy(out, rc)
		if err != nil {
			return err
		}

		out.Close()
		rc.Close()
	}

	return nil
}

func (a *App) downloadFile(url, dst string, progress bool) error {
	a.logger.Infof("Downloading %s to %s", url, dst)
	dst = filepath.Join(a.appDirectory(), dst)
	err := os.MkdirAll(filepath.Dir(dst), 0755)
	if err != nil {
		return err
	}

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return err
	}

	var reader io.Reader = resp.Body
	if progress {
		reader = io.TeeReader(reader, a)
	}

	// Note: KrayAcc serves plain, uncompressed files (no .lzma packing like
	// the CIP client-editor format this launcher originally targeted), so
	// no decompression step is needed here.

	_, err = io.Copy(out, reader)
	if err != nil {
		return err
	}

	atomic.AddInt64(&a.downloadedFiles, 1)

	return nil
}

func (a *App) localExecutable() string {
	name := "Contents/MacOS/client-local"
	if a.OS() == "windows" {
		name = "bin/client-local.exe"
	}
	if a.OS() == "linux" {
		name = "bin/client-local"
	}
	return filepath.Join(a.appDirectory(), name)
}

// executable resolves the game client's .exe inside the installed folder.
// The manifest doesn't say which file is the executable (KrayAcc's schema
// has no such field), so the name comes from viper's "executable" config
// key instead - see readOrCreateConfig in main.go, where it defaults to
// "client.exe" and is written to config.toml on first run so it's easy to
// correct to match the real IMPERAHELPER client filename.
func (a *App) executable() string {
	return filepath.Join(a.appDirectory(), viper.GetString("executable"))
}

func (a *App) Play(local bool) {
	executable := a.executable()
	if local {
		executable = a.localExecutable()
	}
	a.logger.Infof("Launching %s", executable)
	os.Chmod(a.executable(), 0755)
	// Note: the upstream launcher this was based on passed "--battleeye"
	// here, a flag meaningful only to CIP's official client/anti-cheat.
	// Our OTClient-based client doesn't understand it, so it's dropped.
	if err := syscall.Exec(executable, []string{executable}, os.Environ()); err != nil {
		a.logger.Errorf("Failed to launch %s: %s | attempting regular fork", executable, err)
		cmd := exec.Command(executable)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Env = os.Environ()
		if err := cmd.Start(); err != nil {
			a.logger.Errorf("Failed to launch %s: %s", executable, err)
		}
		os.Exit(0)
	}
}

func (a *App) Write(p []byte) (n int, err error) {
	n = len(p)
	atomic.AddInt64(&a.downloadedBytes, int64(n))
	return
}

func sha256Sum(filename string) (string, error) {
	f, err := os.Open(filename)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}
