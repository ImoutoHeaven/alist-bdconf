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
	"sync/atomic"
	"time"

	"github.com/alist-org/alist/v3/drivers/base"
	"github.com/alist-org/alist/v3/pkg/utils"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/semaphore"

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

	// 缓存完整文件
	tempFile, err := stream.CacheFullInTempFile()
	if err != nil {
		return err
	}

	// 基本参数设置
	streamSize := stream.GetSize()
	chunkSize := calculateChunkSize(streamSize)
	count := int(math.Ceil(float64(streamSize) / float64(chunkSize)))

	// 准备并发上传
	uploadBlockList := make([]string, count)
	uploadedChunks := make([]int32, count)
	
	// 创建错误组和并发控制
	eg, uploadCtx := errgroup.WithContext(ctx)
	sem := semaphore.NewWeighted(100) // 设置100并发
	
	// 计算最终的MD5列表互斥锁
	var blockListMutex sync.Mutex
	
	// 启动并发上传任务
	for partseq := 0; partseq < count; partseq++ {
		if utils.IsCanceled(uploadCtx) {
			return uploadCtx.Err()
		}
		
		partseq := partseq // 创建本地变量避免闭包问题
		offset := int64(partseq) * chunkSize
		size := chunkSize
		if partseq == count-1 && streamSize%chunkSize != 0 {
			size = streamSize % chunkSize
		}
		
		eg.Go(func() error {
			// 获取并发信号量
			if err := sem.Acquire(uploadCtx, 1); err != nil {
				return err
			}
			defer sem.Release(1)
			
			// 添加重试逻辑
			var uploadErr error
			for attempt := 0; attempt < 10; attempt++ { // 最多重试10次
				if utils.IsCanceled(uploadCtx) {
					return uploadCtx.Err()
				}
				
				// 读取分块数据
				byteData := make([]byte, size)
				_, err := tempFile.ReadAt(byteData, offset)
				if err != nil && err != io.EOF {
					return err
				}
				
				// 计算MD5
				h := md5.New()
				h.Write(byteData)
				md5sum := hex.EncodeToString(h.Sum(nil))
				
				// 设置上传参数
				u := "https://" + locateupload_resp.Host + "/rest/2.0/pcs/superfile2"
				params := map[string]string{
					"method":     "upload",
					"path":       path,
					"uploadid":   precreateResp.Uploadid,
					"app_id":     "250528",
					"web":        "1",
					"channel":    "dubox",
					"clienttype": "0",
					"partseq":    strconv.Itoa(partseq),
				}
				
				// 执行上传
				res, err := base.RestyClient.R().
					SetContext(uploadCtx).
					SetQueryParams(params).
					SetFileReader("file", stream.GetName(), driver.NewLimitedUploadStream(uploadCtx, bytes.NewReader(byteData))).
					SetHeader("Cookie", d.Cookie).
					Post(u)
				
				if err == nil {
					log.Debugln("分块上传响应:", res.String())
					// 检查响应状态
					statusCode := res.StatusCode()
					errNo := utils.Json.Get(res.Body(), "errno").ToInt()
					
					if statusCode == 200 && (errNo == 0 || errNo == 9 || res.String() == "") {
						// 上传成功
						atomic.StoreInt32(&uploadedChunks[partseq], 1)
						
						// 更新MD5列表
						blockListMutex.Lock()
						uploadBlockList[partseq] = md5sum
						blockListMutex.Unlock()
						
						// 更新进度
						completedChunks := 0
						for i := 0; i < count; i++ {
							if atomic.LoadInt32(&uploadedChunks[i]) == 1 {
								completedChunks++
							}
						}
						up(float64(completedChunks) * 100 / float64(count))
						
						return nil // 成功退出重试循环
					}
					
					uploadErr = fmt.Errorf("块%d上传失败(尝试%d/10), errno: %d, 响应: %s", 
						partseq, attempt+1, errNo, res.String())
				} else {
					uploadErr = fmt.Errorf("块%d上传失败(尝试%d/10): %v", 
						partseq, attempt+1, err)
				}
				
				// 失败则休眠后重试
				log.Debugf("%v", uploadErr)
				time.Sleep(time.Second)
			}
			
			return uploadErr // 返回最后一次尝试的错误
		})
	}
	
	// 等待所有上传任务完成
	if err := eg.Wait(); err != nil {
		return err
	}

	// 创建文件
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
