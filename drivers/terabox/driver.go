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
	// 1. 获取上传服务器地址 - 保持TeraBox原有逻辑
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

	// 2. 计算每个分片的MD5 - 参考百度网盘逻辑
	tempFile, err := stream.CacheFullInTempFile()
	if err != nil {
		return err
	}
	
	streamSize := stream.GetSize()
	chunkSize := calculateChunkSize(streamSize)
	count := int(math.Ceil(float64(streamSize) / float64(chunkSize)))
	
	// 预计算所有分片的MD5
	blockList := make([]string, 0, count)
	h := md5.New()
	buffer := make([]byte, chunkSize)
	
	_, err = tempFile.Seek(0, io.SeekStart)
	if err != nil {
		return err
	}
	
	for i := 0; i < count; i++ {
		size := chunkSize
		if i == count-1 {
			remainingSize := streamSize - int64(i)*chunkSize
			if remainingSize < chunkSize {
				size = remainingSize
			}
		}
		
		buffer = buffer[:size] // 调整buffer大小
		_, err = io.ReadFull(tempFile, buffer)
		if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
			return err
		}
		
		h.Reset()
		h.Write(buffer)
		md5sum := hex.EncodeToString(h.Sum(nil))
		blockList = append(blockList, md5sum)
	}
	
	// 将MD5列表转为JSON字符串
	blockListStr, err := utils.Json.MarshalToString(blockList)
	if err != nil {
		return err
	}
	
	// 3. 重新定位文件开始位置
	_, err = tempFile.Seek(0, io.SeekStart)
	if err != nil {
		return err
	}
	
	// 4. precreate文件 - 保持TeraBox原有API路径和参数
	rawPath := stdpath.Join(dstDir.GetPath(), stream.GetName())
	path := encodeURIComponent(rawPath)
	
	data := map[string]string{
		"path":                  rawPath,
		"autoinit":              "1",
		"target_path":           dstDir.GetPath(),
		"block_list":            blockListStr, // 使用预计算的MD5列表
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
	
	// 5. 并行上传分片 - 参考百度网盘并行上传思路，保持TeraBox参数
	type chunkJob struct {
		partseq int
		offset  int64
		size    int64
	}
	
	type chunkResult struct {
		partseq int
		err     error
	}
	
	jobChan := make(chan chunkJob, count)
	resultChan := make(chan chunkResult, count)
	
	var wg sync.WaitGroup
	
	// 创建50个worker，参考百度网盘实现
	numWorkers := 50
	if count < numWorkers {
		numWorkers = count
	}
	
	fileMutex := &sync.Mutex{}
	
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			
			for job := range jobChan {
				if utils.IsCanceled(ctx) {
					resultChan <- chunkResult{partseq: job.partseq, err: ctx.Err()}
					break
				}
				
				// 读取分片数据
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
				
				if err != nil && err != io.ErrUnexpectedEOF {
					resultChan <- chunkResult{partseq: job.partseq, err: err}
					continue
				}
				
				// 准备TeraBox上传参数
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
				
				// 尝试上传，带重试逻辑
				var uploadErr error
				for retry := 0; retry < 10; retry++ { // 10次重试，参考百度网盘实现
					if utils.IsCanceled(ctx) {
						resultChan <- chunkResult{partseq: job.partseq, err: ctx.Err()}
						break
					}
					
					// 上传分片
					res, err := base.RestyClient.R().
						SetContext(ctx).
						SetQueryParams(params).
						SetFileReader("file", stream.GetName(), driver.NewLimitedUploadStream(ctx, bytes.NewReader(byteData))).
						SetHeader("Cookie", d.Cookie).
						Post(u)
					
					if err == nil && res.StatusCode() >= 200 && res.StatusCode() < 300 {
						// 检查响应中的错误码 - 使用TeraBox的错误码格式
						errno := utils.Json.Get(res.Body(), "errno").ToInt()
						if errno == 0 {
							log.Debugf("Chunk %d uploaded successfully on attempt %d", job.partseq, retry+1)
							uploadErr = nil
							break
						} else {
							uploadErr = fmt.Errorf("error in uploading to terabox, errno=%d", errno)
						}
					} else if err != nil {
						uploadErr = err
					} else {
						uploadErr = fmt.Errorf("status code: %d", res.StatusCode())
					}
					
					log.Debugf("Chunk %d upload attempt %d failed: %v. Retrying after 1 second...", job.partseq, retry+1, uploadErr)
					time.Sleep(1 * time.Second)
				}
				
				resultChan <- chunkResult{partseq: job.partseq, err: uploadErr}
			}
		}()
	}
	
	// 分发任务
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
	
	close(jobChan)
	
	go func() {
		wg.Wait()
		close(resultChan)
	}()
	
	// 收集结果
	completedChunks := 0
	var firstError error
	
	for result := range resultChan {
		if result.err != nil {
			if firstError == nil {
				firstError = result.err
			}
			log.Errorf("Chunk %d upload failed: %v", result.partseq, result.err)
		} else {
			completedChunks++
			
			// 更新进度
			if count > 0 {
				up(float64(completedChunks) * 100 / float64(count))
			}
		}
	}
	
	if firstError != nil {
		return firstError
	}
	
	// 6. 创建文件 - 使用TeraBox的API路径和参数
	params := map[string]string{
		"isdir": "0",
		"rtype": "1", // 1: 重命名冲突文件
	}
	
	data = map[string]string{
		"path":        rawPath,
		"size":        strconv.FormatInt(stream.GetSize(), 10),
		"uploadid":    precreateResp.Uploadid,
		"target_path": dstDir.GetPath(),
		"block_list":  blockListStr, // 使用预计算的MD5列表
		"local_mtime": strconv.FormatInt(stream.ModTime().Unix(), 10),
	}
	
	// 添加重试逻辑
	var createResp CreateResp
	var createErr error
	for retry := 0; retry < 5; retry++ {
		if utils.IsCanceled(ctx) {
			return ctx.Err()
		}
		
		res, err = d.post_form("/api/create", params, data, &createResp)
		if err != nil {
			createErr = err
			log.Debugf("Create file attempt %d failed with error: %v. Retrying in 2 seconds...", retry+1, err)
			time.Sleep(2 * time.Second)
			continue
		}
		
		log.Debugln(string(res))
		
		if createResp.Errno == 0 {
			return nil
		} else {
			if retry == 0 {
				data["mode"] = "1" // 手动上传模式
				log.Debugf("Retrying with mode=1 added")
			} else if retry == 1 {
				params["rtype"] = "3" // 覆盖同名文件
				log.Debugf("Retrying with rtype=3 (overwrite)")
			} else if retry == 2 {
				data["path"] = encodeURIComponent(rawPath)
				log.Debugf("Retrying with URL-encoded path: %s", data["path"])
			}
			
			createErr = fmt.Errorf("[terabox] failed to create file, errno: %d, attempt %d", createResp.Errno, retry+1)
			log.Debugf("%v. Retrying in 2 seconds...", createErr)
			time.Sleep(2 * time.Second)
		}
	}
	
	return createErr
}

var _ driver.Driver = (*Terabox)(nil)
