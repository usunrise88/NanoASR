// Package managed runs NanoASR as the background process of another
// application: started by it, owned by it, and gone when it is.
//
// The application starts `nanoasr managed` as a child process with pipes on
// its standard streams. The child takes a free port on the loopback interface,
// makes up a token, and says both on its standard output as one line of JSON;
// everything after that is the ordinary HTTP API with that token. It stops when
// its standard input closes — which the operating system does for it when the
// application exits, however it exits — or when the process it was told to
// watch is gone.
//
// Nothing here needs administrative rights: the data lives in a directory the
// user owns, the port is above 1024 on the loopback interface, and the process
// only ever lowers its own priority.
//
// What the application chose — which models, which features — is kept beside
// the data in settings.json. The first start has none and says so; the
// application chooses through the API, and every later start comes up with
// what it chose.
package managed

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
)

// APIVersion is the version of this protocol: the events, the settings and
// the managed endpoints. It changes only when an application written against
// an older one would misread a newer one.
const APIVersion = 1

// Paths is where a managed instance keeps everything it writes.
type Paths struct {
	Data     string
	Settings string
	Lock     string
	DB       string
	Models   string
	Spool    string
	Log      string
}

// PathsFor lays out a data directory.
func PathsFor(data string) Paths {
	return Paths{
		Data:     data,
		Settings: filepath.Join(data, "settings.json"),
		Lock:     filepath.Join(data, "nanoasr.lock"),
		DB:       filepath.Join(data, "nanoasr.db"),
		Models:   filepath.Join(data, "models"),
		Spool:    filepath.Join(data, "spool"),
		Log:      filepath.Join(data, "logs", "nanoasr.log"),
	}
}

// Create makes the directories a managed instance writes into.
func (p Paths) Create() error {
	for _, dir := range []string{p.Data, p.Models, p.Spool, filepath.Dir(p.Log)} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	return nil
}

// DefaultDataDir is the per-user directory used when the application names
// none. It is local rather than roaming on Windows: models are gigabytes and
// must not travel with a profile.
//
//	Windows  %LOCALAPPDATA%\NanoASR
//	macOS    ~/Library/Application Support/NanoASR
//	Linux    $XDG_DATA_HOME/nanoasr, else ~/.local/share/nanoasr
func DefaultDataDir() (string, error) {
	switch runtime.GOOS {
	case "windows":
		if dir := os.Getenv("LOCALAPPDATA"); dir != "" {
			return filepath.Join(dir, "NanoASR"), nil
		}
		dir, err := os.UserCacheDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(dir, "NanoASR"), nil
	case "darwin":
		dir, err := os.UserConfigDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(dir, "NanoASR"), nil
	default:
		if dir := os.Getenv("XDG_DATA_HOME"); filepath.IsAbs(dir) {
			return filepath.Join(dir, "nanoasr"), nil
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if home == "" {
			return "", errors.New("no home directory to keep the data in; pass -data-dir")
		}
		return filepath.Join(home, ".local", "share", "nanoasr"), nil
	}
}
