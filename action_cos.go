package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/guoyk93/esbridge/tasks"
	"github.com/guoyk93/iocount"
	"github.com/guoyk93/logutil"
	gzip "github.com/klauspost/pgzip"
	"github.com/olivere/elastic"
	"github.com/tencentyun/cos-go-sdk-v5"
	"io"
	"log"
	"strings"
)

func COSSearch(clientCOS *cos.Client, keyword string) (err error) {
	log.Printf("在腾讯云存储搜索: %s", keyword)
	splits := strings.Split(keyword, ",")
	for i, s := range splits {
		splits[i] = strings.TrimSpace(s)
	}
	var marker string
	var res *cos.BucketGetResult
	for {
		if res, _, err = clientCOS.Bucket.Get(context.Background(), &cos.BucketGetOptions{
			Marker: marker,
		}); err != nil {
			return
		}
	outerLoop:
		for _, o := range res.Contents {
			if !strings.HasSuffix(o.Key, tasks.ExtCompressedNDJSON) {
				log.Printf("发现未知文件: %s", o.Key)
				continue
			}
			p := strings.TrimPrefix(strings.TrimSuffix(o.Key, tasks.ExtCompressedNDJSON), "/")
			for _, s := range splits {
				if !strings.Contains(p, s) {
					continue outerLoop
				}
			}
			ss := strings.Split(p, "/")
			if len(ss) != 2 {
				log.Printf("发现未知文件: %s", o.Key)
				continue
			}
			log.Printf("找到 INDEX = %s, PROJECT = %s, SIZE = %02f", ss[0], ss[1], float64(o.Size)/1000000.0)
		}
		if res.IsTruncated {
			marker = res.NextMarker
		} else {
			return
		}
	}
}

func COSCheckFile(clientCOS *cos.Client, index, project string) (err error) {
	log.Printf("检查腾讯云存储文件: INDEX = %s, PROJECT = %s", index, project)
	_, err = clientCOS.Object.Head(context.Background(), index+"/"+project+tasks.ExtCompressedNDJSON, nil)
	return
}

func COSImportToES(clientCOS *cos.Client, index, project string, clientES *elastic.Client) (err error) {
	title := fmt.Sprintf("从腾讯云存储恢复索引: %s (%s)", index, project)
	log.Printf(title)
	var res *cos.Response
	if res, err = clientCOS.Object.Get(context.Background(), index+"/"+project+tasks.ExtCompressedNDJSON, nil); err != nil {
		return
	}
	defer res.Body.Close()

	prg := logutil.NewProgress(logutil.LoggerFunc(log.Printf), title)
	prg.SetTotal(res.ContentLength)

	cr := iocount.NewReader(res.Body)
	var zr *gzip.Reader
	if zr, err = gzip.NewReader(cr); err != nil {
		return
	}
	br := bufio.NewReader(zr)

	var bs *elastic.BulkService

	commit := func(force bool) (err error) {
		if bs != nil {
			if force || bs.NumberOfActions() > 4000 {
				var res *elastic.BulkResponse
				if res, err = bs.Do(context.Background()); err != nil {
					return
				}
				failed := res.Failed()
				if len(failed) > 0 {
					buf, _ := json.MarshalIndent(failed[0], "", "  ")
					err = fmt.Errorf("存在失败的索引请求: %s", string(buf))
					return
				}
			}
		}
		return
	}

	var buf []byte
	for {
		if buf, err = br.ReadBytes('\n'); err != nil {
			if err == io.EOF {
				err = nil
			}
			break
		}

		buf = bytes.TrimSpace(buf)

		if len(buf) > 0 {
			if bs == nil {
				bs = clientES.Bulk()
			}
			bs.Add(elastic.NewBulkIndexRequest().Index(index).Type("_doc").Doc(json.RawMessage(buf)))
		}

		if err = commit(false); err != nil {
			return
		}

		prg.SetCount(cr.ReadCount())
	}

	if err = commit(true); err != nil {
		return
	}

	return
}
