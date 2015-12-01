package tarsum

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"io"
	"strings"

	"github.com/jlhawn/tarsum/archive/tar"

	log "github.com/Sirupsen/logrus"
)

const (
	buf8K  = 8 * 1024
	buf16K = 16 * 1024
	buf32K = 32 * 1024
)

// NewTarSum creates a new interface for calculating a fixed time checksum of a
// tar archive.
//
// This is used for calculating checksums of layers of an image, in some cases
// including the byte payload of the image's json metadata as well, and for
// calculating the checksums for buildcache.
func newTarSum(r io.Reader, dc bool, v Version) (*tarSum, error) {
	return newTarSumHash(r, dc, v, defaultTHash)
}

// Create a new TarSum, providing a THash to use rather than the DefaultTHash
func newTarSumHash(r io.Reader, dc bool, v Version, th tHash) (*tarSum, error) {
	headerSelector, err := getTarHeaderSelector(v)
	if err != nil {
		return nil, err
	}
	ts := &tarSum{Reader: r, DisableCompression: dc, tarSumVersion: v, headerSelector: headerSelector, th: th}
	err = ts.initTarSum()
	return ts, err
}

// tarSum struct is the structure for a Version0 checksum calculation
type tarSum struct {
	io.Reader
	tarR               *tar.Reader
	tarW               *tar.Writer
	writer             writeCloseFlusher
	bufTar             *bytes.Buffer
	bufWriter          *bytes.Buffer
	bufData            []byte
	h                  hash.Hash
	th                 tHash
	sums               fileInfoSums
	fileCounter        int64
	currentFile        string
	finished           bool
	first              bool
	DisableCompression bool              // false by default. When false, the output gzip compressed.
	tarSumVersion      Version           // this field is not exported so it can not be mutated during use
	headerSelector     tarHeaderSelector // handles selecting and ordering headers for files in the archive
}

func (ts tarSum) Hash() tHash {
	return ts.th
}

func (ts tarSum) Version() Version {
	return ts.tarSumVersion
}

// A hash.Hash type generator and its name
type tHash interface {
	Hash() hash.Hash
	Name() string
}

// Convenience method for creating a THash
func newTHash(name string, h func() hash.Hash) tHash {
	return simpleTHash{n: name, h: h}
}

// TarSum default is "sha256"
var defaultTHash = newTHash("sha256", sha256.New)

type simpleTHash struct {
	n string
	h func() hash.Hash
}

func (sth simpleTHash) Name() string    { return sth.n }
func (sth simpleTHash) Hash() hash.Hash { return sth.h() }

func (ts *tarSum) encodeHeader(h *tar.Header) error {
	for _, elem := range ts.headerSelector.selectHeaders(h) {
		if _, err := ts.h.Write([]byte(elem[0] + elem[1])); err != nil {
			return err
		}
	}
	return nil
}

func (ts *tarSum) initTarSum() error {
	ts.bufTar = bytes.NewBuffer([]byte{})
	ts.bufWriter = bytes.NewBuffer([]byte{})
	ts.tarR = tar.NewReader(ts.Reader)
	ts.tarW = tar.NewWriter(ts.bufTar)
	if !ts.DisableCompression {
		ts.writer = gzip.NewWriter(ts.bufWriter)
	} else {
		ts.writer = &nopCloseFlusher{Writer: ts.bufWriter}
	}
	if ts.th == nil {
		ts.th = defaultTHash
	}
	ts.h = ts.th.Hash()
	ts.h.Reset()
	ts.first = true
	ts.sums = fileInfoSums{}
	return nil
}

func (ts *tarSum) Read(buf []byte) (int, error) {
	if ts.finished {
		return ts.bufWriter.Read(buf)
	}
	if len(ts.bufData) < len(buf) {
		switch {
		case len(buf) <= buf8K:
			ts.bufData = make([]byte, buf8K)
		case len(buf) <= buf16K:
			ts.bufData = make([]byte, buf16K)
		case len(buf) <= buf32K:
			ts.bufData = make([]byte, buf32K)
		default:
			ts.bufData = make([]byte, len(buf))
		}
	}
	buf2 := ts.bufData[:len(buf)]

	n, err := ts.tarR.Read(buf2)
	if err != nil {
		if err == io.EOF {
			if _, err := ts.h.Write(buf2[:n]); err != nil {
				return 0, err
			}
			if !ts.first {
				ts.sums = append(ts.sums, fileInfoSum{name: ts.currentFile, sum: hex.EncodeToString(ts.h.Sum(nil)), pos: ts.fileCounter})
				ts.fileCounter++
				ts.h.Reset()
			} else {
				ts.first = false
			}

			currentHeader, err := ts.tarR.Next()
			if err != nil {
				if err == io.EOF {
					if err := ts.tarW.Close(); err != nil {
						return 0, err
					}
					if _, err := io.Copy(ts.writer, ts.bufTar); err != nil {
						return 0, err
					}
					if err := ts.writer.Close(); err != nil {
						return 0, err
					}
					ts.finished = true
					return n, nil
				}
				return n, err
			}
			ts.currentFile = strings.TrimSuffix(strings.TrimPrefix(currentHeader.Name, "./"), "/")
			if err := ts.encodeHeader(currentHeader); err != nil {
				return 0, err
			}
			if err := ts.tarW.WriteHeader(currentHeader); err != nil {
				return 0, err
			}
			if _, err := ts.tarW.Write(buf2[:n]); err != nil {
				return 0, err
			}
			ts.tarW.Flush()
			if _, err := io.Copy(ts.writer, ts.bufTar); err != nil {
				return 0, err
			}
			ts.writer.Flush()

			return ts.bufWriter.Read(buf)
		}
		return n, err
	}

	// Filling the hash buffer
	if _, err = ts.h.Write(buf2[:n]); err != nil {
		return 0, err
	}

	// Filling the tar writter
	if _, err = ts.tarW.Write(buf2[:n]); err != nil {
		return 0, err
	}
	ts.tarW.Flush()

	// Filling the output writer
	if _, err = io.Copy(ts.writer, ts.bufTar); err != nil {
		return 0, err
	}
	ts.writer.Flush()

	return ts.bufWriter.Read(buf)
}

func (ts *tarSum) Sum(extra []byte) string {
	ts.sums.SortBySums()
	h := ts.th.Hash()
	if extra != nil {
		h.Write(extra)
	}
	for _, fis := range ts.sums {
		log.Debugf("-->%s<--", fis.Sum())
		h.Write([]byte(fis.Sum()))
	}
	checksum := ts.Version().String() + "+" + ts.th.Name() + ":" + hex.EncodeToString(h.Sum(nil))
	log.Debugf("checksum processed: %s", checksum)
	return checksum
}

func (ts *tarSum) GetSums() fileInfoSums {
	return ts.sums
}