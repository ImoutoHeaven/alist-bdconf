func (d *Terabox) Put(ctx context.Context, dstDir model.Obj, stream model.FileStreamer, up driver.UpdateProgress) error {
    // Locate upload server - 保持不变
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

    // Precreate file - 保持不变
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

    // 并行上传分片 - 关键修改部分
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
    
    // 创建文件 - 关键点：与原始代码保持一致，使用实际上传的MD5值
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
            createErr = fmt.Errorf("[terabox] failed to create file, errno: %d, attempt %d", createResp.Errno, retry+1)
            log.Debugf("%v. Retrying in 2 seconds...", createErr)
            time.Sleep(2 * time.Second)
        }
    }
    
    return createErr
}
