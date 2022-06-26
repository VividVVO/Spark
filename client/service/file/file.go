package file

import (
	"archive/zip"
	"errors"
	"github.com/VividVVO/Spark/client/config"
	"github.com/imroc/req/v3"
	"io"
	"io/ioutil"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

type File struct {
	Name string `json:"name"`
	Size uint64 `json:"size"`
	Time int64  `json:"time"`
	Type int    `json:"type"` // 0: file, 1: folder, 2: volume
}

// listFiles returns files and directories find in path.
func listFiles(path string) ([]File, error) {
	result := make([]File, 0)
	files, err := ioutil.ReadDir(path)
	if err != nil {
		return nil, err
	}
	for i := 0; i < len(files); i++ {
		itemType := 0
		if files[i].IsDir() {
			itemType = 1
		}
		result = append(result, File{
			Name: files[i].Name(),
			Size: uint64(files[i].Size()),
			Time: files[i].ModTime().Unix(),
			Type: itemType,
		})
	}
	return result, nil
}

// FetchFile saves file from bridge to local.
// Save body as temp file and when done, rename it to file.
func FetchFile(dir, file, bridge string) error {
	url := config.GetBaseURL(false) + `/api/bridge/pull`
	client := req.C().DisableAutoReadResponse()
	resp, err := client.R().SetQueryParam(`bridge`, bridge).Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// If dest file exists, write to temp file first.
	dest := path.Join(dir, file)
	tmpFile := dest
	destExists := false
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		tmpFile = getTempFileName(dir, file)
		destExists = true
	}

	fh, err := os.Create(tmpFile)
	if err != nil {
		return err
	}
	for {
		buf := make([]byte, 1024)
		n, err := resp.Body.Read(buf)
		if err != nil && err != io.EOF {
			fh.Truncate(0)
			fh.Close()
			os.Remove(tmpFile)
			return err
		}
		if n == 0 {
			break
		}
		_, err = fh.Write(buf[:n])
		if err != nil {
			fh.Truncate(0)
			fh.Close()
			os.Remove(tmpFile)
			return err
		}
		fh.Sync()
	}
	fh.Close()

	// Delete old file if exists.
	// Then rename temp file to file.
	if destExists {
		os.Remove(dest)
		err = os.Rename(tmpFile, dest)
	}
	return err
}

func RemoveFiles(files []string) error {
	for i := 0; i < len(files); i++ {
		if files[i] == `\` || files[i] == `/` || len(files[i]) == 0 {
			return errors.New(`${i18n|fileOrDirNotExist}`)
		}
		err := os.RemoveAll(files[i])
		if err != nil {
			return err
		}
	}
	return nil
}

func UploadFiles(files []string, bridge string, start, end int64) error {
	uploadReq := req.R()
	reader, writer := io.Pipe()
	if len(files) == 1 {
		stat, err := os.Stat(files[0])
		if err != nil {
			return err
		}
		if stat.IsDir() {
			err = uploadMulti(files, writer, uploadReq)
		} else {
			err = uploadSingle(files[0], start, end, writer, uploadReq)
		}
		if err != nil {
			return err
		}
	} else {
		err := uploadMulti(files, writer, uploadReq)
		if err != nil {
			return err
		}
	}
	url := config.GetBaseURL(false) + `/api/bridge/push`
	_, err := uploadReq.
		SetBody(reader).
		SetQueryParam(`bridge`, bridge).
		Send(`PUT`, url)
	reader.Close()
	return err
}

func uploadSingle(path string, start, end int64, writer *io.PipeWriter, req *req.Request) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	stat, err := file.Stat()
	if err != nil {
		file.Close()
		return err
	}
	size := stat.Size()
	req.SetHeaders(map[string]string{
		`FileName`: stat.Name(),
		`FileSize`: strconv.FormatInt(size, 10),
	})
	if size < end {
		return errors.New(`${i18n|invalidFileSize}`)
	}
	if end == 0 {
		req.RawRequest.ContentLength = size - start
	} else {
		req.RawRequest.ContentLength = end - start
	}
	shouldRead := req.RawRequest.ContentLength
	file.Seek(start, 0)
	go func() {
		for {
			bufSize := int64(2 << 14)
			if shouldRead < bufSize {
				bufSize = shouldRead
			}
			buffer := make([]byte, bufSize) // 32768
			n, err := file.Read(buffer)
			buffer = buffer[:n]
			shouldRead -= int64(n)
			writer.Write(buffer)
			if n == 0 || shouldRead == 0 || err != nil {
				break
			}
		}
		writer.Close()
		file.Close()
	}()
	return nil
}

func uploadMulti(files []string, writer *io.PipeWriter, req *req.Request) error {
	type Job struct {
		info      os.FileInfo
		path      string
		hierarchy []string
	}
	const QueueSize = 64
	var fails []string
	var escape = false
	var queue = make(chan Job, QueueSize)
	var lock = &sync.Mutex{}
	if len(files) == 1 {
		req.SetHeader(`FileName`, path.Base(strings.ReplaceAll(files[0], `\`, `/`))+`.zip`)
	} else {
		req.SetHeader(`FileName`, `Archive.zip`)
	}
	zipWriter := zip.NewWriter(writer)
	archiveFile := func(job Job) {
		file, err := os.Open(job.path)
		if err != nil {
			fails = append(fails, job.path)
			return
		}
		relativePath := strings.Join(job.hierarchy, `/`)
		fileWriter, err := zipWriter.Create(relativePath)
		if err != nil {
			fails = append(fails, job.path)
			file.Close()
			return
		}
		for {
			eof := false
			buf := make([]byte, 2<<14) // 32768
			n, err := file.Read(buf)
			if err != nil {
				eof = err == io.EOF
				if !eof {
					fails = append(fails, job.path)
					break
				}
			}
			if n == 0 {
				break
			}
			_, err = fileWriter.Write(buf[:n])
			if err != nil {
				fails = append(fails, job.path)
				escape = true
				break
			}
			if eof {
				break
			}
		}
		file.Close()
	}
	scanDir := func(job Job) {
		entries, err := ioutil.ReadDir(job.path)
		if err != nil {
			fails = append(fails, job.path)
			return
		}
		spare := make([]Job, 0)
		for _, entry := range entries {
			if escape {
				break
			}
			subJob := Job{entry, path.Join(job.path, entry.Name()), append(job.hierarchy, entry.Name())}
			if entry.IsDir() {
				lock.Lock()
				if len(queue) < QueueSize {
					queue <- subJob
				} else {
					spare = append(spare, subJob)
				}
				lock.Unlock()
			} else {
				archiveFile(subJob)
			}
		}
		go func() {
			for _, subJob := range spare {
				lock.Lock()
				queue <- subJob
				lock.Unlock()
			}
		}()
	}
	handleJob := func(job Job) {
		if job.info.IsDir() {
			scanDir(job)
		} else {
			archiveFile(job)
		}
	}
	pushItems := func(items []string) error {
		for i := 0; i < len(items); i++ {
			if escape {
				break
			}
			stat, err := os.Stat(items[i])
			if err != nil {
				fails = append(fails, items[i])
				return err
			}
			lock.Lock()
			queue <- Job{stat, items[i], []string{stat.Name()}}
			lock.Unlock()
		}
		return nil
	}
	if len(files) > QueueSize {
		pushItems(files[:QueueSize])
		go pushItems(files[QueueSize:])
	} else {
		pushItems(files)
	}
	go func() {
		for !escape {
			select {
			case job := <-queue:
				if escape {
					break
				}
				handleJob(job)
				if escape {
					// Try to get next job, to make locking producer unlock.
					// Escaping now, job is useless so there's no need to keep it.
					_, _ = <-queue
				}
			default:
				escape = true
				_, _ = <-queue
				break
			}
			if escape {
				lock.Lock()
				close(queue)
				lock.Unlock()
				if len(fails) > 0 {
					zipWriter.SetComment(`Those files could not be archived:` + "\n" + strings.Join(fails, "\n"))
				}
				zipWriter.Close()
				writer.Close()
				break
			}
		}
	}()
	return nil
}

func UploadTextFile(path, bridge string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	uploadReq := req.R()
	stat, err := file.Stat()
	if err != nil {
		return err
	}
	size := stat.Size()
	// Check if size larger than 2MB.
	if size > 2<<20 {
		return errors.New(`${i18n|fileTooLarge}`)
	}
	uploadReq.SetHeaders(map[string]string{
		`FileName`: stat.Name(),
		`FileSize`: strconv.FormatInt(size, 10),
	})
	uploadReq.RawRequest.ContentLength = size

	// Check file if is a text file.
	// UTF-8 and GBK are only supported yet.
	buf := make([]byte, size)
	_, err = file.Read(buf)
	if err != nil {
		return err
	}
	if utf8.Valid(buf) {
		uploadReq.SetHeader(`FileEncoding`, `UTF-8`)
	} else if gbkValidate(buf) {
		uploadReq.SetHeader(`FileEncoding`, `GBK`)
	} else {
		return errors.New(`${i18n|fileEncodingUnsupported}`)
	}

	file.Seek(0, 0)
	url := config.GetBaseURL(false) + `/api/bridge/push`
	_, err = uploadReq.
		SetBody(file).
		SetQueryParam(`bridge`, bridge).
		Send(`PUT`, url)
	return err
}

func gbkValidate(b []byte) bool {
	length := len(b)
	var i int = 0
	for i < length {
		if b[i] <= 0x7f {
			i++
			continue
		} else {
			if i+1 < length {
				if b[i] >= 0x81 && b[i] <= 0xfe && b[i+1] >= 0x40 && b[i+1] <= 0xfe && b[i+1] != 0xf7 {
					i += 2
					continue
				}
			}
			return false
		}
	}
	return true
}

func getTempFileName(dir, file string) string {
	exists := true
	tempFile := ``
	for i := 0; exists; i++ {
		tempFile = path.Join(dir, file+`.tmp.`+strconv.Itoa(i))
		_, err := os.Stat(tempFile)
		if os.IsNotExist(err) {
			exists = false
		}
	}
	return tempFile
}
