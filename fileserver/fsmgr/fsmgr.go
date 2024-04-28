// Package fsmgr manages fs objects
package fsmgr

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/haiwen/seafile-server/fileserver/objstore"
	"github.com/haiwen/seafile-server/fileserver/utils"
	jsoniter "github.com/json-iterator/go"

	"github.com/dgraph-io/ristretto"
)

var json = jsoniter.ConfigCompatibleWithStandardLibrary

// Seafile is a file object
type Seafile struct {
	data     []byte
	Version  int      `json:"version"`
	FileType int      `json:"type"`
	FileID   string   `json:"-"`
	FileSize uint64   `json:"size"`
	BlkIDs   []string `json:"block_ids"`
}

// In the JSON encoding generated by C language, there are spaces after the ',' and ':', and the order of the fields is sorted by the key.
// So it is not compatible with the json library generated by go.
func (file *Seafile) toJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	buf.WriteString("\"block_ids\": [")
	for i, blkID := range file.BlkIDs {
		data, err := json.Marshal(blkID)
		if err != nil {
			return nil, err
		}
		buf.Write(data)
		if i < len(file.BlkIDs)-1 {
			buf.WriteByte(',')
			buf.WriteByte(' ')
		}
	}
	buf.WriteByte(']')
	buf.WriteByte(',')
	buf.WriteByte(' ')

	data, err := json.Marshal(file.FileSize)
	if err != nil {
		return nil, err
	}
	writeField(&buf, "\"size\"", data)
	buf.WriteByte(',')
	buf.WriteByte(' ')

	data, err = json.Marshal(SeafMetadataTypeFile)
	if err != nil {
		return nil, err
	}
	writeField(&buf, "\"type\"", data)
	buf.WriteByte(',')
	buf.WriteByte(' ')

	data, err = json.Marshal(file.Version)
	if err != nil {
		return nil, err
	}
	writeField(&buf, "\"version\"", data)

	buf.WriteByte('}')

	return buf.Bytes(), nil
}

func writeField(buf *bytes.Buffer, key string, value []byte) {
	buf.WriteString(key)
	buf.WriteByte(':')
	buf.WriteByte(' ')
	buf.Write(value)
}

// SeafDirent is a dir entry object
type SeafDirent struct {
	Mode     uint32 `json:"mode"`
	ID       string `json:"id"`
	Name     string `json:"name"`
	Mtime    int64  `json:"mtime"`
	Modifier string `json:"modifier"`
	Size     int64  `json:"size"`
}

func (dent *SeafDirent) toJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	data, err := json.Marshal(dent.ID)
	if err != nil {
		return nil, err
	}
	writeField(&buf, "\"id\"", data)
	buf.WriteByte(',')
	buf.WriteByte(' ')

	data, err = json.Marshal(dent.Mode)
	if err != nil {
		return nil, err
	}
	writeField(&buf, "\"mode\"", data)
	buf.WriteByte(',')
	buf.WriteByte(' ')

	if IsRegular(dent.Mode) {
		data, err = jsonNoEscape(dent.Modifier)
		if err != nil {
			return nil, err
		}
		writeField(&buf, "\"modifier\"", data)
		buf.WriteByte(',')
		buf.WriteByte(' ')
	}

	data, err = json.Marshal(dent.Mtime)
	if err != nil {
		return nil, err
	}
	writeField(&buf, "\"mtime\"", data)
	buf.WriteByte(',')
	buf.WriteByte(' ')

	data, err = jsonNoEscape(dent.Name)
	if err != nil {
		return nil, err
	}
	writeField(&buf, "\"name\"", data)

	if IsRegular(dent.Mode) {
		buf.WriteByte(',')
		buf.WriteByte(' ')
		data, err = json.Marshal(dent.Size)
		if err != nil {
			return nil, err
		}
		writeField(&buf, "\"size\"", data)
	}
	buf.WriteByte('}')

	return buf.Bytes(), nil
}

// In golang json, the string is encoded using HTMLEscape, which replaces "<", ">", "&", U+2028, and U+2029 are escaped to "\u003c","\u003e", "\u0026", "\u2028", and "\u2029".
// So it is not compatible with the json library generated by c. This replacement can be disabled when using an Encoder, by calling SetEscapeHTML(false).
func jsonNoEscape(data interface{}) ([]byte, error) {
	var buf bytes.Buffer

	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)

	if err := encoder.Encode(data); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// SeafDir is a dir object
type SeafDir struct {
	data    []byte
	Version int           `json:"version"`
	DirType int           `json:"type"`
	DirID   string        `json:"-"`
	Entries []*SeafDirent `json:"dirents"`
}

func (dir *SeafDir) toJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	buf.WriteString("\"dirents\": [")
	for i, entry := range dir.Entries {
		data, err := entry.toJSON()
		if err != nil {
			return nil, err
		}
		buf.Write(data)
		if i < len(dir.Entries)-1 {
			buf.WriteByte(',')
			buf.WriteByte(' ')
		}
	}
	buf.WriteByte(']')
	buf.WriteByte(',')
	buf.WriteByte(' ')

	data, err := json.Marshal(SeafMetadataTypeDir)
	if err != nil {
		return nil, err
	}
	writeField(&buf, "\"type\"", data)
	buf.WriteByte(',')
	buf.WriteByte(' ')

	data, err = json.Marshal(dir.Version)
	if err != nil {
		return nil, err
	}
	writeField(&buf, "\"version\"", data)

	buf.WriteByte('}')

	return buf.Bytes(), nil
}

// FileCountInfo contains information of files
type FileCountInfo struct {
	FileCount int64
	Size      int64
	DirCount  int64
}

// Meta data type of dir or file
const (
	SeafMetadataTypeInvalid = iota
	SeafMetadataTypeFile
	SeafMetadataTypeLink
	SeafMetadataTypeDir
)

var store *objstore.ObjectStore

// Empty value of sha1
const (
	EmptySha1 = "0000000000000000000000000000000000000000"
)

// Since zlib library allocates a large amount of memory every time a new reader is created, when the number of calls is too large,
// the GC will be executed frequently, resulting in high CPU usage.
var zlibReaders []io.ReadCloser
var zlibLock sync.Mutex

// Add fs cache, on the one hand to avoid repeated creation and destruction of repeatedly accessed objects,
// on the other hand it will also slow down the speed at which objects are released.
var fsCache *ristretto.Cache

// Init initializes fs manager and creates underlying object store.
func Init(seafileConfPath string, seafileDataDir string, fsCacheLimit int64) {
	store = objstore.New(seafileConfPath, seafileDataDir, "fs")
	fsCache, _ = ristretto.NewCache(&ristretto.Config{
		NumCounters: 1e7,          // number of keys to track frequency of (10M).
		MaxCost:     fsCacheLimit, // maximum cost of cache.
		BufferItems: 64,           // number of keys per Get buffer.
		Cost:        calCost,
	})
}

func calCost(value interface{}) int64 {
	return sizeOf(value)
}

const (
	sizeOfString     = int64(unsafe.Sizeof(string("")))
	sizeOfPointer    = int64(unsafe.Sizeof(uintptr(0)))
	sizeOfSeafile    = int64(unsafe.Sizeof(Seafile{}))
	sizeOfSeafDir    = int64(unsafe.Sizeof(SeafDir{}))
	sizeOfSeafDirent = int64(unsafe.Sizeof(SeafDirent{}))
)

func sizeOf(a interface{}) int64 {
	var size int64
	switch x := a.(type) {
	case string:
		return sizeOfString + int64(len(x))
	case []string:
		for _, s := range x {
			size += sizeOf(s)
		}
		return size
	case *Seafile:
		size = sizeOfPointer
		size += sizeOfSeafile
		size += int64(len(x.FileID))
		size += sizeOf(x.BlkIDs)
		return size
	case *SeafDir:
		size = sizeOfPointer
		size += sizeOfSeafDir
		size += int64(len(x.DirID))
		for _, dent := range x.Entries {
			size += sizeOf(dent)
		}
		return size
	case *SeafDirent:
		size = sizeOfPointer
		size += sizeOfSeafDirent
		size += int64(len(x.ID))
		size += int64(len(x.Name))
		size += int64(len(x.Modifier))
		return size

	}
	return 0
}

func initZlibReader() (io.ReadCloser, error) {
	var buf bytes.Buffer

	// Since the corresponding reader has not been obtained when zlib is initialized,
	// an io.Reader needs to be built to initialize zlib.
	w := zlib.NewWriter(&buf)
	w.Close()

	r, err := zlib.NewReader(&buf)
	if err != nil {
		return nil, err
	}

	return r, nil
}

// GetOneZlibReader gets a zlib reader from zlibReaders.
func GetOneZlibReader() io.ReadCloser {
	zlibLock.Lock()
	defer zlibLock.Unlock()
	var reader io.ReadCloser
	if len(zlibReaders) == 0 {
		reader, err := initZlibReader()
		if err != nil {
			return nil
		}
		return reader
	}
	reader = zlibReaders[0]
	zlibReaders = zlibReaders[1:]

	return reader
}

func ReturnOneZlibReader(reader io.ReadCloser) {
	if reader == nil {
		return
	}
	zlibLock.Lock()
	defer zlibLock.Unlock()
	zlibReaders = append(zlibReaders, reader)
}

// NewDirent initializes a SeafDirent object
func NewDirent(id string, name string, mode uint32, mtime int64, modifier string, size int64) *SeafDirent {
	dent := new(SeafDirent)
	dent.ID = id
	if id == "" {
		dent.ID = EmptySha1
	}
	dent.Name = name
	dent.Mode = mode
	dent.Mtime = mtime
	if IsRegular(mode) {
		dent.Modifier = modifier
		dent.Size = size
	}

	return dent
}

// NewSeafdir initializes a SeafDir object
func NewSeafdir(version int, entries []*SeafDirent) (*SeafDir, error) {
	dir := new(SeafDir)
	dir.Version = version
	dir.Entries = entries
	if len(entries) == 0 {
		dir.DirID = EmptySha1
		return dir, nil
	}
	jsonstr, err := dir.toJSON()
	if err != nil {
		err := fmt.Errorf("failed to convert seafdir to json")
		return nil, err
	}
	dir.data = jsonstr
	checksum := sha1.Sum(jsonstr)
	dir.DirID = hex.EncodeToString(checksum[:])

	return dir, nil
}

// NewSeafile initializes a Seafile object
func NewSeafile(version int, fileSize int64, blkIDs []string) (*Seafile, error) {
	seafile := new(Seafile)
	seafile.Version = version
	seafile.FileSize = uint64(fileSize)
	seafile.BlkIDs = blkIDs
	if len(blkIDs) == 0 {
		seafile.FileID = EmptySha1
		return seafile, nil
	}

	jsonstr, err := seafile.toJSON()
	if err != nil {
		err := fmt.Errorf("failed to convert seafile to json")
		return nil, err
	}
	seafile.data = jsonstr
	checkSum := sha1.Sum(jsonstr)
	seafile.FileID = hex.EncodeToString(checkSum[:])

	return seafile, nil
}

func uncompress(p []byte, reader io.ReadCloser) ([]byte, error) {
	b := bytes.NewReader(p)
	var out bytes.Buffer

	if reader == nil {
		r, err := zlib.NewReader(b)
		if err != nil {
			return nil, err
		}

		_, err = io.Copy(&out, r)
		if err != nil {
			r.Close()
			return nil, err
		}

		r.Close()
		return out.Bytes(), nil
	}

	// resue the old zlib reader.
	resetter, _ := reader.(zlib.Resetter)
	err := resetter.Reset(b, nil)
	if err != nil {
		return nil, err
	}

	_, err = io.Copy(&out, reader)
	if err != nil {
		return nil, err
	}

	return out.Bytes(), nil
}

func compress(p []byte) ([]byte, error) {
	var out bytes.Buffer
	w := zlib.NewWriter(&out)

	_, err := w.Write(p)
	if err != nil {
		w.Close()
		return nil, err
	}

	w.Close()

	return out.Bytes(), nil
}

// FromData reads from p and converts JSON-encoded data to Seafile.
func (seafile *Seafile) FromData(p []byte, reader io.ReadCloser) error {
	b, err := uncompress(p, reader)
	if err != nil {
		return err
	}
	err = json.Unmarshal(b, seafile)
	if err != nil {
		return err
	}

	if seafile.FileType != SeafMetadataTypeFile {
		return fmt.Errorf("object %s is not a file", seafile.FileID)
	}
	if seafile.Version < 1 {
		return fmt.Errorf("seafile object %s version should be > 0, version is %d", seafile.FileID, seafile.Version)
	}
	if seafile.BlkIDs == nil {
		return fmt.Errorf("no block id array in seafile object %s", seafile.FileID)
	}
	for _, blkID := range seafile.BlkIDs {
		if !utils.IsObjectIDValid(blkID) {
			return fmt.Errorf("block id %s is invalid", blkID)
		}
	}

	return nil
}

// ToData converts seafile to JSON-encoded data and writes to w.
func (seafile *Seafile) ToData(w io.Writer) error {
	buf, err := compress(seafile.data)
	if err != nil {
		return err
	}

	_, err = w.Write(buf)
	if err != nil {
		return err
	}

	return nil
}

// ToData converts seafdir to JSON-encoded data and writes to w.
func (seafdir *SeafDir) ToData(w io.Writer) error {
	buf, err := compress(seafdir.data)
	if err != nil {
		return err
	}

	_, err = w.Write(buf)
	if err != nil {
		return err
	}

	return nil
}

// FromData reads from p and converts JSON-encoded data to SeafDir.
func (seafdir *SeafDir) FromData(p []byte, reader io.ReadCloser) error {
	b, err := uncompress(p, reader)
	if err != nil {
		return err
	}
	err = json.Unmarshal(b, seafdir)
	if err != nil {
		return err
	}
	if seafdir.DirType != SeafMetadataTypeDir {
		return fmt.Errorf("object %s is not a dir", seafdir.DirID)
	}
	if seafdir.Version < 1 {
		return fmt.Errorf("dir object %s version should be > 0, version is %d", seafdir.DirID, seafdir.Version)
	}
	if seafdir.Entries == nil {
		return fmt.Errorf("no dirents in dir object %s", seafdir.DirID)
	}
	for _, dent := range seafdir.Entries {
		if !utils.IsObjectIDValid(dent.ID) {
			return fmt.Errorf("dirent id %s is invalid", dent.ID)
		}
	}

	return nil
}

// ReadRaw reads data in binary format from storage backend.
func ReadRaw(repoID string, objID string, w io.Writer) error {
	err := store.Read(repoID, objID, w)
	if err != nil {
		return err
	}

	return nil
}

// WriteRaw writes data in binary format to storage backend.
func WriteRaw(repoID string, objID string, r io.Reader) error {
	err := store.Write(repoID, objID, r, false)
	if err != nil {
		return err
	}
	return nil
}

// GetSeafile gets seafile from storage backend.
func GetSeafile(repoID string, fileID string) (*Seafile, error) {
	return getSeafile(repoID, fileID, nil)
}

// GetSeafileWithZlibReader gets seafile from storage backend with a zlib reader.
func GetSeafileWithZlibReader(repoID string, fileID string, reader io.ReadCloser) (*Seafile, error) {
	return getSeafile(repoID, fileID, reader)
}

func getSeafile(repoID string, fileID string, reader io.ReadCloser) (*Seafile, error) {

	var buf bytes.Buffer
	seafile := new(Seafile)
	if fileID == EmptySha1 {
		seafile.FileID = EmptySha1
		return seafile, nil
	}

	seafile.FileID = fileID

	err := ReadRaw(repoID, fileID, &buf)
	if err != nil {
		errors := fmt.Errorf("failed to read seafile object from storage : %v", err)
		return nil, errors
	}

	err = seafile.FromData(buf.Bytes(), reader)
	if err != nil {
		errors := fmt.Errorf("failed to parse seafile object %s/%s : %v", repoID, fileID, err)
		return nil, errors
	}

	if seafile.Version < 1 {
		errors := fmt.Errorf("seafile object %s/%s version should be > 0", repoID, fileID)
		return nil, errors
	}

	return seafile, nil
}

// SaveSeafile saves seafile to storage backend.
func SaveSeafile(repoID string, seafile *Seafile) error {
	fileID := seafile.FileID
	if fileID == EmptySha1 {
		return nil
	}

	exist, _ := store.Exists(repoID, fileID)
	if exist {
		return nil
	}

	seafile.FileType = SeafMetadataTypeFile
	var buf bytes.Buffer
	err := seafile.ToData(&buf)
	if err != nil {
		errors := fmt.Errorf("failed to convert seafile object %s/%s to json", repoID, fileID)
		return errors
	}

	err = WriteRaw(repoID, fileID, &buf)
	if err != nil {
		errors := fmt.Errorf("failed to write seafile object to storage : %v", err)
		return errors
	}

	return nil
}

// GetSeafdir gets seafdir from storage backend.
func GetSeafdir(repoID string, dirID string) (*SeafDir, error) {
	return getSeafdir(repoID, dirID, nil, false)
}

// GetSeafdir gets seafdir from storage backend with a zlib reader.
func GetSeafdirWithZlibReader(repoID string, dirID string, reader io.ReadCloser) (*SeafDir, error) {
	return getSeafdir(repoID, dirID, reader, true)
}

func getSeafdir(repoID string, dirID string, reader io.ReadCloser, useCache bool) (*SeafDir, error) {
	var seafdir *SeafDir
	if useCache {
		seafdir = getSeafdirFromCache(repoID, dirID)
		if seafdir != nil {
			return seafdir, nil
		}
	}
	var buf bytes.Buffer
	seafdir = new(SeafDir)
	if dirID == EmptySha1 {
		seafdir.DirID = EmptySha1
		return seafdir, nil
	}

	seafdir.DirID = dirID

	err := ReadRaw(repoID, dirID, &buf)
	if err != nil {
		errors := fmt.Errorf("failed to read seafdir object from storage : %v", err)
		return nil, errors
	}

	err = seafdir.FromData(buf.Bytes(), reader)
	if err != nil {
		errors := fmt.Errorf("failed to parse seafdir object %s/%s : %v", repoID, dirID, err)
		return nil, errors
	}

	if seafdir.Version < 1 {
		errors := fmt.Errorf("seadir object %s/%s version should be > 0", repoID, dirID)
		return nil, errors
	}

	if useCache {
		setSeafdirToCache(repoID, seafdir)
	}

	return seafdir, nil
}

func getSeafdirFromCache(repoID string, dirID string) *SeafDir {
	key := repoID + dirID
	v, ok := fsCache.Get(key)
	if !ok {
		return nil
	}
	seafdir, ok := v.(*SeafDir)
	if ok {
		return seafdir
	}

	return nil
}

func setSeafdirToCache(repoID string, seafdir *SeafDir) error {
	key := repoID + seafdir.DirID
	fsCache.SetWithTTL(key, seafdir, 0, time.Duration(1*time.Hour))

	return nil
}

// SaveSeafdir saves seafdir to storage backend.
func SaveSeafdir(repoID string, seafdir *SeafDir) error {
	dirID := seafdir.DirID
	if dirID == EmptySha1 {
		return nil
	}
	exist, _ := store.Exists(repoID, dirID)
	if exist {
		return nil
	}

	seafdir.DirType = SeafMetadataTypeDir
	var buf bytes.Buffer
	err := seafdir.ToData(&buf)
	if err != nil {
		errors := fmt.Errorf("failed to convert seafdir object %s/%s to json", repoID, dirID)
		return errors
	}

	err = WriteRaw(repoID, dirID, &buf)
	if err != nil {
		errors := fmt.Errorf("failed to write seafdir object to storage : %v", err)
		return errors
	}

	return nil
}

// Exists check if fs object is exists.
func Exists(repoID string, objID string) (bool, error) {
	if objID == EmptySha1 {
		return true, nil
	}
	return store.Exists(repoID, objID)
}

func comp(c rune) bool {
	return c == '/'
}

// IsDir check if the mode is dir.
func IsDir(m uint32) bool {
	return (m & syscall.S_IFMT) == syscall.S_IFDIR
}

// IsRegular Check if the mode is regular.
func IsRegular(m uint32) bool {
	return (m & syscall.S_IFMT) == syscall.S_IFREG
}

// ErrPathNoExist is an error indicating that the file does not exist
var ErrPathNoExist = fmt.Errorf("path does not exist")

// GetSeafdirByPath gets the object of seafdir by path.
func GetSeafdirByPath(repoID string, rootID string, path string) (*SeafDir, error) {
	dir, err := GetSeafdir(repoID, rootID)
	if err != nil {
		errors := fmt.Errorf("directory is missing")
		return nil, errors
	}

	path = filepath.Join("/", path)
	parts := strings.FieldsFunc(path, comp)
	var dirID string
	for _, name := range parts {
		entries := dir.Entries
		for _, v := range entries {
			if v.Name == name && IsDir(v.Mode) {
				dirID = v.ID
				break
			}
		}

		if dirID == `` {
			return nil, ErrPathNoExist
		}

		dir, err = GetSeafdir(repoID, dirID)
		if err != nil {
			errors := fmt.Errorf("directory is missing")
			return nil, errors
		}
	}

	return dir, nil
}

// GetSeafdirIDByPath gets the dirID of SeafDir by path.
func GetSeafdirIDByPath(repoID, rootID, path string) (string, error) {
	dirID, mode, err := GetObjIDByPath(repoID, rootID, path)
	if err != nil {
		err := fmt.Errorf("failed to get dir id by path: %s: %w", path, err)
		return "", err
	}
	if dirID == "" || !IsDir(mode) {
		return "", nil
	}

	return dirID, nil
}

// GetObjIDByPath gets the obj id by path
func GetObjIDByPath(repoID, rootID, path string) (string, uint32, error) {
	var name string
	var baseDir *SeafDir
	formatPath := filepath.Join(path)
	if len(formatPath) == 0 || formatPath == "/" {
		return rootID, syscall.S_IFDIR, nil
	}
	index := strings.Index(formatPath, "/")
	if index < 0 {
		dir, err := GetSeafdir(repoID, rootID)
		if err != nil {
			err := fmt.Errorf("failed to find root dir %s: %v", rootID, err)
			return "", 0, err
		}
		name = formatPath
		baseDir = dir
	} else {
		name = filepath.Base(formatPath)
		dirName := filepath.Dir(formatPath)
		dir, err := GetSeafdirByPath(repoID, rootID, dirName)
		if err != nil {
			if err == ErrPathNoExist {
				return "", syscall.S_IFDIR, ErrPathNoExist
			}
			err := fmt.Errorf("failed to find dir %s in repo %s: %v", dirName, repoID, err)
			return "", syscall.S_IFDIR, err
		}
		baseDir = dir
	}

	entries := baseDir.Entries
	for _, de := range entries {
		if de.Name == name {
			return de.ID, de.Mode, nil
		}
	}

	return "", 0, nil

}

// GetFileCountInfoByPath gets the count info of file by path.
func GetFileCountInfoByPath(repoID, rootID, path string) (*FileCountInfo, error) {
	dirID, err := GetSeafdirIDByPath(repoID, rootID, path)
	if err != nil {
		err := fmt.Errorf("failed to get file count info for repo %s path %s: %v", repoID, path, err)
		return nil, err
	}

	info, err := getFileCountInfo(repoID, dirID)
	if err != nil {
		err := fmt.Errorf("failed to get file count in repo %s: %v", repoID, err)
		return nil, err
	}

	return info, nil
}

func getFileCountInfo(repoID, dirID string) (*FileCountInfo, error) {
	dir, err := GetSeafdir(repoID, dirID)
	if err != nil {
		err := fmt.Errorf("failed to get dir: %v", err)
		return nil, err
	}

	info := new(FileCountInfo)

	entries := dir.Entries
	for _, de := range entries {
		if IsDir(de.Mode) {
			tmpInfo, err := getFileCountInfo(repoID, de.ID)
			if err != nil {
				err := fmt.Errorf("failed to get file count: %v", err)
				return nil, err
			}
			info.DirCount = tmpInfo.DirCount + 1
			info.FileCount += tmpInfo.FileCount
			info.Size += tmpInfo.Size
		} else {
			info.FileCount++
			info.Size += de.Size
		}
	}

	return info, nil
}