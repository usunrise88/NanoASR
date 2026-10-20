package isolation

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"time"

	"github.com/usunrise88/nanoasr/internal/audio"
	"github.com/usunrise88/nanoasr/internal/core"
)

// writePCM stores samples as raw 16-bit PCM, half the disk of float32 and the
// exact payload of the WAV they are served as.
func writePCM(path string, x []float32) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return core.Errorf(core.CodeInternal, "cannot write the isolation's audio").WithCause(err)
	}
	const block = 1 << 16
	for at := 0; at < len(x); at += block {
		if _, err := f.Write(audio.EncodePCM16(x[at:min(at+block, len(x))])); err != nil {
			_ = f.Close()
			return core.Errorf(core.CodeInternal, "cannot write the isolation's audio").WithCause(err)
		}
	}
	if err := f.Close(); err != nil {
		return core.Errorf(core.CodeInternal, "cannot write the isolation's audio").WithCause(err)
	}
	return nil
}

func readPCM(path string) ([]float32, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, core.Errorf(core.CodeInternal, "cannot read the isolation's audio").WithCause(err)
	}
	x := make([]float32, len(b)/2)
	for i := range x {
		x[i] = float32(int16(binary.LittleEndian.Uint16(b[2*i:]))) / 32768
	}
	return x, nil
}

// writeJSON replaces a file whole: a reader, or a restart, sees the old
// content or the new and never half of one.
func writeJSON(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return core.Errorf(core.CodeInternal, "cannot encode %s", path).WithCause(err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return core.Errorf(core.CodeInternal, "cannot write %s", path).WithCause(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return core.Errorf(core.CodeInternal, "cannot write %s", path).WithCause(err)
	}
	return nil
}

// wavFile is a stored PCM file read as WAV: a header, then the file. It
// seeks, so a player can ask for any part of it (a Range request) and start
// playing before the rest has arrived.
type wavFile struct {
	header []byte
	f      *os.File
	size   int64
	off    int64
	mod    time.Time
}

func openWAV(path string) (*wavFile, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, core.Errorf(core.CodeIsolationBusy, "the isolated voice is gone").WithCause(err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, core.Errorf(core.CodeInternal, "cannot read the isolated voice").WithCause(err)
	}
	header := audio.WAVHeader(SampleRate, info.Size())
	return &wavFile{header: header, f: f, size: int64(len(header)) + info.Size(), mod: info.ModTime()}, nil
}

func (w *wavFile) Read(p []byte) (int, error) {
	if w.off >= w.size {
		return 0, io.EOF
	}
	h := int64(len(w.header))
	if w.off < h {
		n := copy(p, w.header[w.off:])
		w.off += int64(n)
		return n, nil
	}
	n, err := w.f.ReadAt(p, w.off-h)
	w.off += int64(n)
	if err == io.EOF && n > 0 {
		err = nil
	}
	return n, err
}

func (w *wavFile) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		offset += w.off
	case io.SeekEnd:
		offset += w.size
	default:
		return 0, errors.New("wav: bad whence")
	}
	if offset < 0 {
		return 0, errors.New("wav: seek before the start")
	}
	w.off = offset
	return offset, nil
}

func (w *wavFile) Close() error { return w.f.Close() }

// Size is the length of the whole WAV, for Content-Length.
func (w *wavFile) Size() int64 { return w.size }

// ModTime is when the audio was written: a voice isolated again is a new
// file, and a player that cached the old one must not keep playing it.
func (w *wavFile) ModTime() time.Time { return w.mod }

// memWAV is a WAV held in memory, a voice's sample: it seeks as a file does.
type memWAV struct{ *bytes.Reader }

func (memWAV) Close() error { return nil }

func wavReader(x []float32) io.ReadCloser {
	data := audio.EncodePCM16(x)
	return memWAV{bytes.NewReader(append(audio.WAVHeader(SampleRate, int64(len(data))), data...))}
}

// resultSource hands a track to the job queue as an upload.
type resultSource struct {
	path, name string
	size       int64
}

func (r *resultSource) Open() (io.ReadCloser, error) { return openWAV(r.path) }
func (r *resultSource) Filename() string             { return r.name }
func (r *resultSource) Size() int64                  { return r.size }
func (r *resultSource) Close() error                 { return nil }
