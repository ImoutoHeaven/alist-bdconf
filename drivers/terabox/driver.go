package terabox

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	stdpath "path"
	"strconv"
	"sync"
	"time"

	"github.com/alist-org/alist/v3/drivers/base"
	"github.com/alist-org/alist/v3/pkg/utils"
	log "github.com/sirupsen/logrus"

	"github.com/alist-org/alist/v3/internal/driver"
	"github.com/alist-org/alist/v3/internal/model"
)

type Terabox struct {
	model.Storage
	Addition
	JsToken           string
	url_domain_prefix string
	base_url          string
}

func (d *Terabox) Config() driver.Config {
	return config
}

func (d *Terabox) GetAddition() driver.Additional {
	return &d.Addition
}

func (d *Terabox) Init(ctx context.Context) error {
	var resp CheckLoginResp
	d.base_url = "https://www.terabox.com"
	d.url_domain_prefix = "jp"
	_, err := d.get("/api/check/login", nil, &resp)
	if err != nil {
		return err
	}
	if resp.Errno != 0 {
		if resp.Errno == 9000 {
			return fmt.Errorf("terabox is not yet available in this area")
		}
		return fmt.Errorf("failed to check login status according to cookie")
	}
	return err
}

func (d *Terabox) Drop(ctx context.Context) error {
	return nil
}

func (d *Terabox) List(ctx context.Context, dir model.Obj, args model.ListArgs) ([]model.Obj, error) {
	files, err := d.getFiles(dir.GetPath())
	if err != nil {
		return nil, err
	}
	return utils.SliceConvert(files, func(src File) (model.Obj, error) {
		return fileToObj(src), nil
	})
}

func (d *Terabox) Link(ctx context.Context, file model.Obj, args model.LinkArgs) (*model.Link, error) {
	if d.DownloadAPI == "crack" {
		return d.linkCrack(file, args)
	}
	return d.linkOfficial(file, args)
}

func (d *Terabox) MakeDir(ctx context.Context, parentDir model.Obj, dirName string) error {
	params := map[string]string{
		"a": "commit",
	}
	data := map[string]string{
		"path":       stdpath.Join(parentDir.GetPath(), dirName),
		"isdir":      "1",
		"block_list": "[]",
	}
	res, err := d.post_form("/api/create", params, data, nil)
	log.Debugln(string(res))
	return err
}

func (d *Terabox) Move(ctx context.Context, srcObj, dstDir model.Obj) error {
	data := []base.Json{
		{
			"path":    srcObj.GetPath(),
			"dest":    dstDir.GetPath(),
			"newname": srcObj.GetName(),
		},
	}
	_, err := d.manage("move", data)
	return err
}

func (d *Terabox) Rename(ctx context.Context, srcObj model.Obj, newName string) error {
	data := []base.Json{
		{
			"path":    srcObj.GetPath(),
			"newname": newName,
		},
	}
	_, err := d.manage("rename", data)
	return err
}

func (d *Terabox) Copy(ctx context.Context, srcObj, dstDir model.Obj) error {
	data := []base.Json{
		{
			"path":    srcObj.GetPath(),
			"dest":    dstDir.GetPath(),
			"newname": srcObj.GetName(),
		},
	}
	_, err := d.manage("copy", data)
	return err
}

func (d *Terabox) Remove(ctx context.Context, obj model.Obj) error {
	data := []string{obj.GetPath()}
	_, err := d.manage("delete", data)
	return err
}

func (d *Terabox) Put(ctx context.Context, dstDir model.Obj, stream model.FileStreamer, up driver.UpdateProgress) error {
	resp, err := base.RestyClient.R().
		SetContext(ctx).
		Get("https://" + d.url_domain_prefix + "-data.terabox.com/rest/2.0/pcs/file?method=locateupload")
	if err != nil {
		return err
	}
	var locateupload_resp LocateUploadResp
	err = utils.Json.Unmarshal(resp.Body(), &locateupload_resp)
	if err != nil {
		log.Debugln(resp)
		return err
	}
	log.Debugln(locateupload_resp)

	// precreate file
	rawPath := stdpath.Join(dstDir.GetPath(), stream.GetName())
	path := encodeURIComponent(rawPath)

	var precreateBlockListStr string
	if stream.GetSize() > initialChunkSize {
		precreateBlockListStr = `["5910a591dd8fc18c32a8f3df4fdc1761","a5fc157d78e6ad1c7e114b056c92821e"]`
	} else {
		precreateBlockListStr = `["5910a591dd8fc18c32a8f3df4fdc1761"]`
	}

	data := map[string]string{
		"path":                  rawPath,
		"autoinit":              "1",
		"target_path":           dstDir.GetPath(),
		"block_list":            precreateBlockListStr,
		"local_mtime":           strconv.FormatInt(stream.ModTime().Unix(), 10),
		"file_limit_switch_v34": "true",
	}
	var precreateResp PrecreateResp
	log.Debugln(data)
	res, err := d.post_form("/api/precreate", nil, data, &precreateResp)
	if err != nil {
		return err
	}
	log.Debugf("%+v", precreateResp)
	if precreateResp.Errno != 0 {
		log.Debugln(string(res))
		return fmt.Errorf("[terabox] failed to precreate file, errno: %d", precreateResp.Errno)
	}
	if precreateResp.ReturnType == 2 {
		return nil
	}

	// Enhanced chunk upload with parallelism and retry logic
	tempFile, err := stream.CacheFullInTempFile()
	if err != nil {
		return err
	}
	
	streamSize := stream.GetSize()
	chunkSize := calculateChunkSize(streamSize)
	count := int(math.Ceil(float64(streamSize) / float64(chunkSize)))
	
	// Create channels for job distribution and result collection
	type chunkJob struct {
		partseq int
		offset  int64
		size    int64
	}
	
	type chunkResult struct {
		partseq int
		md5sum  string
		err     error
	}
	
	jobChan := make(chan chunkJob, count)
	resultChan := make(chan chunkResult, count)
	
	// Create a waitgroup to wait for all workers to finish
	var wg sync.WaitGroup
	
	// Create worker pool (up to 16 workers)
	numWorkers := 16
	if count < numWorkers {
		numWorkers = count
	}
	
	// Start workers
	fileMutex := &sync.Mutex{} // Mutex to synchronize file access
	
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			
			for job := range jobChan {
				if utils.IsCanceled(ctx) {
					resultChan <- chunkResult{partseq: job.partseq, err: ctx.Err()}
					break // Exit loop on context cancellation
				}
				
				// Read chunk data with mutex protection
				byteData := make([]byte, job.size)
				
				fileMutex.Lock()
				_, err := tempFile.Seek(job.offset, io.SeekStart)
				if err != nil {
					fileMutex.Unlock()
					resultChan <- chunkResult{partseq: job.partseq, err: err}
					continue
				}
				
				_, err = io.ReadFull(tempFile, byteData)
				fileMutex.Unlock()
				
				if err != nil {
					resultChan <- chunkResult{partseq: job.partseq, err: err}
					continue
				}
				
				// Calculate MD5
				h := md5.New()
				h.Write(byteData)
				md5sum := hex.EncodeToString(h.Sum(nil))
				
				// Prepare parameters for this chunk
				params := map[string]string{
					"method":     "upload",
					"path":       path,
					"uploadid":   precreateResp.Uploadid,
					"app_id":     "250528",
					"web":        "1",
					"channel":    "dubox",
					"clienttype": "0",
					"partseq":    strconv.Itoa(job.partseq),
				}
				
				u := "https://" + locateupload_resp.Host + "/rest/2.0/pcs/superfile2"
				
				// Try uploading with retries
				var uploadErr error
				for retry := 0; retry < 10; retry++ {
					if utils.IsCanceled(ctx) {
						resultChan <- chunkResult{partseq: job.partseq, err: ctx.Err()}
						break
					}
					
					// Upload the chunk
					res, err := base.RestyClient.R().
						SetContext(ctx).
						SetQueryParams(params).
						SetFileReader("file", stream.GetName(), driver.NewLimitedUploadStream(ctx, bytes.NewReader(byteData))).
						SetHeader("Cookie", d.Cookie).
						Post(u)
					
					if err == nil {
						// Check if the response indicates success
						respBody := res.Body()
						var respJson map[string]interface{}
						if err := utils.Json.Unmarshal(respBody, &respJson); err == nil {
							if errno, ok := respJson["errno"].(float64); ok && errno == 0 {
								// Upload successful
								log.Debugf("Chunk %d uploaded successfully on attempt %d", job.partseq, retry+1)
								uploadErr = nil
								break
							} else {
								uploadErr = fmt.Errorf("chunk upload failed with errno: %.0f", errno)
							}
						} else {
							uploadErr = fmt.Errorf("failed to parse response: %s", string(respBody))
						}
					} else {
						uploadErr = err
					}
					
					// If we get here, the upload failed
					log.Debugf("Chunk %d upload attempt %d failed: %v. Retrying after 1 second...", job.partseq, retry+1, uploadErr)
					
					// Sleep before retrying
					time.Sleep(1 * time.Second)
				}
				
				// Send the result
				if uploadErr != nil {
					resultChan <- chunkResult{partseq: job.partseq, err: fmt.Errorf("failed to upload chunk %d after 10 retries: %w", job.partseq, uploadErr)}
				} else {
					resultChan <- chunkResult{partseq: job.partseq, md5sum: md5sum, err: nil}
				}
			}
		}()
	}
	
	// Queue jobs
	for partseq := 0; partseq < count; partseq++ {
		offset := int64(partseq) * chunkSize
		size := chunkSize
		if offset+size > streamSize {
			size = streamSize - offset
		}
		
		jobChan <- chunkJob{
			partseq: partseq,
			offset:  offset,
			size:    size,
		}
	}
	
	// Close the job channel to signal workers to exit
	close(jobChan)
	
	// Start a goroutine to close the result channel when all workers are done
	go func() {
		wg.Wait()
		close(resultChan)
	}()
	
	// Collect results and handle errors
	uploadBlockList := make([]string, count)
	completedChunks := 0
	var firstError error
	
	for result := range resultChan {
		if result.err != nil {
			if firstError == nil {
				firstError = result.err
			}
			log.Errorf("Chunk %d upload failed: %v", result.partseq, result.err)
		} else {
			uploadBlockList[result.partseq] = result.md5sum
			completedChunks++
			
			// Update progress
			if count > 0 {
				up(float64(completedChunks) * 100 / float64(count))
			}
		}
	}
	
	// Check if any chunk failed
	if firstError != nil {
		return firstError
	}
	
	// Create file (existing code)
	params := map[string]string{
		"isdir": "0",
		"rtype": "1",
	}

	uploadBlockListStr, err := utils.Json.MarshalToString(uploadBlockList)
	if err != nil {
		return err
	}
	data = map[string]string{
		"path":        rawPath,
		"size":        strconv.FormatInt(stream.GetSize(), 10),
		"uploadid":    precreateResp.Uploadid,
		"target_path": dstDir.GetPath(),
		"block_list":  uploadBlockListStr,
		"local_mtime": strconv.FormatInt(stream.ModTime().Unix(), 10),
	}
	var createResp CreateResp
	res, err = d.post_form("/api/create", params, data, &createResp)
	log.Debugln(string(res))
	if err != nil {
		return err
	}
	if createResp.Errno != 0 {
		return fmt.Errorf("[terabox] failed to create file, errno: %d", createResp.Errno)
	}
	return nil
}

var _ driver.Driver = (*Terabox)(nil)
