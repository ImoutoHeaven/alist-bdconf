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
	// 1. 定位上传服务器
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

	// 2. 预创建文件
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

	// 3. 并行上传分片
	tempFile, err := stream.CacheFullInTempFile()
	if err != nil {
		return err
	}
	
	streamSize := stream.GetSize()
	chunkSize := calculateChunkSize(streamSize)
	count := int(math.Ceil(float64(streamSize) / float64(chunkSize)))
	
	// 创建通道和等待组
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
	
	var wg sync.WaitGroup
	
	// 创建worker池
	numWorkers := 10  // 降低并发数，避免可能的服务器限制
	if count < numWorkers {
		numWorkers = count
	}
	
	fileMutex := &sync.Mutex{}
	
	// 启动workers
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
				
				// 计算MD5 - 与原始代码一致
				h := md5.New()
				h.Write(byteData)
				md5sum := hex.EncodeToString(h.Sum(nil))
				
				// 关键点：使用与原始代码一致的partseq策略
				params := map[string]string{
					"method":     "upload",
					"path":       path,
					"uploadid":   precreateResp.Uploadid,
					"app_id":     "250528",
					"web":        "1",
					"channel":    "dubox",
					"clienttype": "0",
					"partseq":    strconv.Itoa(job.partseq),  // 使用job.partseq
				}
				
				u := "https://" + locateupload_resp.Host + "/rest/2.0/pcs/superfile2"
				
				// 上传分片，含重试逻辑
				var uploadErr error
				for retry := 0; retry < 5; retry++ {
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
						log.Debugf("Chunk %d uploaded successfully on attempt %d", job.partseq, retry+1)
						uploadErr = nil
						break
					} else if err != nil {
						uploadErr = err
					} else {
						uploadErr = fmt.Errorf("status code: %d", res.StatusCode())
					}
					
					log.Debugf("Chunk %d upload attempt %d failed: %v. Retrying...", job.partseq, retry+1, uploadErr)
					time.Sleep(1 * time.Second)
				}
				
				if uploadErr != nil {
					resultChan <- chunkResult{partseq: job.partseq, err: uploadErr}
				} else {
					resultChan <- chunkResult{partseq: job.partseq, md5sum: md5sum, err: nil}
				}
			}
		}()
	}
	
	// 分发任务 - 使用简单的从0开始的序列作为partseq
	for partseq := 0; partseq < count; partseq++ {
		offset := int64(partseq) * chunkSize
		size := chunkSize
		
		// 调整最后一个分片的大小
		if partseq == count-1 || offset+size > streamSize {
			size = streamSize - offset
		}
		
		jobChan <- chunkJob{
			partseq: partseq,  // 简单使用索引作为partseq，与原始代码一致
			offset:  offset,
			size:    size,
		}
	}
	
	close(jobChan)
	
	go func() {
		wg.Wait()
		close(resultChan)
	}()
	
	// 收集结果并按顺序保存MD5值
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
			uploadBlockList[result.partseq] = result.md5sum  // 将MD5保存在正确的位置
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
	
	// 4. 创建文件 - 关键点：与原始代码保持一致，使用实际上传的MD5值
	params := map[string]string{
		"isdir": "0",
		"rtype": "1",
	}

	// 将MD5列表转为JSON字符串
	uploadBlockListStr, err := utils.Json.MarshalToString(uploadBlockList)
	if err != nil {
		return err
	}
	
	// 使用与原始代码相同的参数
	data = map[string]string{
		"path":        rawPath,
		"size":        strconv.FormatInt(stream.GetSize(), 10),
		"uploadid":    precreateResp.Uploadid,
		"target_path": dstDir.GetPath(),
		"block_list":  uploadBlockListStr,  // 使用实际计算的MD5值列表
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
			// 尝试不同的修复策略
			if retry == 0 {
				// 第一次失败后，尝试添加额外参数
				data["target_path"] = dstDir.GetPath()
				log.Debugf("Retrying with target_path added: %s", data["target_path"])
			} else if retry == 1 {
				// 第二次失败后，尝试使用URL编码的路径
				data["path"] = encodeURIComponent(rawPath)
				log.Debugf("Retrying with URL-encoded path: %s", data["path"])
			} else if retry == 2 {
				// 第三次失败后，尝试更改重命名策略
				params["rtype"] = "3" // 覆盖同名文件
				log.Debugf("Retrying with rtype=3 (overwrite)")
			} else if retry == 3 {
				// 第四次失败，尝试使用原始预创建的block_list
				data["block_list"] = precreateBlockListStr
				log.Debugf("Retrying with original precreate block_list")
			}
			
			createErr = fmt.Errorf("[terabox] failed to create file, errno: %d, attempt %d", createResp.Errno, retry+1)
			log.Debugf("%v. Retrying in 2 seconds...", createErr)
			time.Sleep(2 * time.Second)
		}
	}
	
	return createErr
}

var _ driver.Driver = (*Terabox)(nil)
