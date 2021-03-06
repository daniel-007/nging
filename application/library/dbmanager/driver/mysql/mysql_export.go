/*
   Nging is a toolbox for webmasters
   Copyright (C) 2018-present  Wenhui Shen <swh@admpub.com>

   This program is free software: you can redistribute it and/or modify
   it under the terms of the GNU Affero General Public License as published
   by the Free Software Foundation, either version 3 of the License, or
   (at your option) any later version.

   This program is distributed in the hope that it will be useful,
   but WITHOUT ANY WARRANTY; without even the implied warranty of
   MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
   GNU Affero General Public License for more details.

   You should have received a copy of the GNU Affero General Public License
   along with this program.  If not, see <https://www.gnu.org/licenses/>.
*/

package mysql

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/admpub/archiver"
	"github.com/admpub/errors"
	loga "github.com/admpub/log"
	"github.com/admpub/nging/application/handler"
	"github.com/admpub/nging/application/library/cron"
	"github.com/admpub/nging/application/library/dbmanager/driver"
	"github.com/webx-top/com"
	"github.com/webx-top/db"
	"github.com/webx-top/echo"
)

/*
mysqldump 参数说明：
-d 			结构(--no-data:不导出任何数据，只导出数据库表结构)
-t 			数据(--no-create-info:只导出数据，而不添加CREATE TABLE 语句)
-n 			(--no-create-db:只导出数据，而不添加CREATE DATABASE 语句）
-R 			(--routines:导出存储过程以及自定义函数)
-E 			(--events:导出事件)
--triggers 	(默认导出触发器，使用--skip-triggers屏蔽导出)
-B 			(--databases:导出数据库列表，单个库时可省略）
--tables 	表列表（单个表时可省略）
*/

var (
	cleanRegExp = regexp.MustCompile(` AUTO_INCREMENT=[0-9]*\s*`)
	backgrounds = sync.Map{} //后台导入导出任务
	// SQLTempDir sql文件缓存目录获取函数(用于导入导出SQL)
	SQLTempDir = os.TempDir
)

const (
	OpExport = `export`
	OpImport = `import`
)

type FileInfos []*FileInfo

type FileInfo struct {
	Start      time.Time
	End        time.Time
	Elapsed    time.Duration
	Path       string
	Size       int64
	Compressed bool
	Error      string
}

func TempDir(op string) string {
	dir := filepath.Join(SQLTempDir(), `dbmanager/cache`, op)
	if !com.IsDir(dir) {
		err := os.MkdirAll(dir, os.ModePerm)
		if err != nil {
			loga.Error(err)
		}
	}
	return dir
}

// Export 导出SQL文件
func Export(ctx context.Context, cfg *driver.DbAuth, tables []string, structWriter, dataWriter interface{}, resetAutoIncrements ...bool) error {
	if len(tables) == 0 {
		return errors.New(`No table selected for export`)
	}
	log.Println(`Starting backup:`, tables)
	var (
		port, host         string
		resetAutoIncrement bool
	)
	if len(resetAutoIncrements) > 0 {
		resetAutoIncrement = resetAutoIncrements[0]
	}
	if p := strings.LastIndex(cfg.Host, `:`); p > 0 {
		host = cfg.Host[0:p]
		port = cfg.Host[p+1:]
	} else {
		host = cfg.Host
	}
	if len(port) == 0 {
		port = `3306`
	}
	if !com.InSlice(cfg.Charset, Charsets) {
		return errors.New(`字符集charset值无效`)
	}
	args := []string{
		"--default-character-set=" + cfg.Charset,
		"--single-transaction",
		//"--column-statistics=0",//低版本不支持
		"--set-gtid-purged=OFF",
		"--no-autocommit",
		//"--ignore-table="+cfg.Db+".details",
		//"--ignore-table="+cfg.Db+".large_table2",
		"--opt",
		"-d", //加上此参数代表只导出表结构，不导出数据
		"-h" + host,
		"-P" + port,
		"-u" + cfg.Username,
		"-p" + cfg.Password,
		cfg.Db,
		//"--result-file=/root/backup.sql",
	}
	clean := func(w io.Writer) {
		if c, y := w.(io.Closer); y {
			c.Close()
		}
	}
	var typeOptIndex int
	for index, value := range args {
		if value == `-d` {
			typeOptIndex = index
			break
		}
	}
	//args = append(args, `--tables`)
	args = append(args, tables...)
	rec := cron.NewCmdRec(1000)
	for index, writer := range []interface{}{structWriter, dataWriter} {
		if writer == nil {
			continue
		}
		if index > 0 {
			args[typeOptIndex] = `-t` //导出数据
		}
		//log.Println(`mysqldump`, strings.Join(args, ` `))
		cmd := exec.CommandContext(ctx, "mysqldump", args...)
		cmd.Stderr = rec
		var (
			w        io.Writer
			err      error
			onFinish func() error
		)
		switch v := writer.(type) {
		case io.Writer:
			w = v
		case string:
			dir := filepath.Dir(v)
			if _, err := os.Stat(dir); os.IsNotExist(err) {
				err = os.MkdirAll(dir, os.ModePerm)
				if err != nil {
					return fmt.Errorf(`Failed to backup: %v`, err)
				}
			}
			w, err = os.Create(v)
			if err != nil {
				return fmt.Errorf(`Failed to backup: %v`, err)
			}
			onFinish = func() error {
				if index > 0 {
					return nil
				}
				if resetAutoIncrement {
					return ResetAutoIncrement(v)
				}
				return nil
			}
		default:
			return errors.Wrapf(db.ErrUnsupported, `SQL Writer Error: %T`, v)
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			clean(w)
			return fmt.Errorf(`Failed to backup: %v`, err)
		}
		if err := cmd.Start(); err != nil {
			stdout.Close()
			clean(w)
			return fmt.Errorf(`Failed to backup: %v`, err)
		}
		if _, err := io.Copy(w, stdout); err != nil {
			stdout.Close()
			clean(w)
			return fmt.Errorf(`Failed to backup: %v`, err)
		}
		if err := cmd.Wait(); err != nil {
			stdout.Close()
			clean(w)
			return errors.New(err.Error() + `: ` + rec.String())
		}
		clean(w)
		stdout.Close()
		if onFinish != nil {
			if err = onFinish(); err != nil {
				return err
			}
		}
	}
	return nil
}

// ResetAutoIncrement 重置AUTO_INCREMENT值为0
func ResetAutoIncrement(sqlStructFile string) error {
	b, err := ioutil.ReadFile(sqlStructFile)
	if err != nil {
		return err
	}
	b = cleanRegExp.ReplaceAll(b, []byte(` `))
	return ioutil.WriteFile(sqlStructFile, b, 0666)
}

func (m *mySQL) getCharsetList() []string {
	e, _ := m.getCharsets()
	cs := make([]string, 0)
	for k := range e {
		cs = append(cs, k)
	}
	sort.Strings(cs)
	return cs
}

func (m *mySQL) Export() error {
	//fmt.Printf("%#v\n", m.getCharsetList())
	var err error
	if m.IsPost() {
		var tables []string
		if len(m.dbName) == 0 {
			m.fail(m.T(`请选择数据库`))
			return m.returnTo(m.GenURL(`listDb`))
		}
		if m.Formx(`all`).Bool() {
			var ok bool
			tables, ok = m.Get(`tableList`).([]string)
			if !ok {
				tables, err = m.getTables()
				if err != nil {
					m.fail(err.Error())
					return m.returnTo(m.GenURL(`export`))
				}
			}
		} else {
			tables = m.FormValues(`table`)
			if len(tables) == 1 && len(tables[0]) > 0 {
				tables = strings.Split(tables[0], `,`)
			}
			views := m.FormValues(`view`)
			if len(views) == 1 && len(views[0]) > 0 {
				views = append(tables, strings.Split(views[0], `,`)...)
			}
			if len(views) > 0 {
				tables = append(tables, views...)
			}
		}
		output := m.Form(`output`)
		types := m.FormValues(`type`)
		cacheKey := com.Md5(com.Dump([]interface{}{tables, output, types}, false))

		var exports map[string]*FileInfos
		if old, exists := backgrounds.Load(OpExport); exists {
			exports = old.(map[string]*FileInfos)
		} else {
			exports = map[string]*FileInfos{}
		}
		if _, ok := exports[cacheKey]; ok {
			return errors.New(m.T(`任务正在后台处理中，请稍候...`))
		}

		var (
			structWriter, dataWriter interface{}
			sqlFiles                 []string
			async                    bool
			fileInfos                = &FileInfos{}
		)
		exports[cacheKey] = fileInfos
		nowTime := com.String(time.Now().Unix())
		saveDir := TempDir(`export`)
		switch output {
		case `down`:
			m.Response().Header().Set(echo.HeaderContentDisposition, fmt.Sprintf("attachment; filename=%q", m.dbName+"-sql-"+nowTime+".sql"))
			fallthrough
		case `open`:
			if com.InSlice(`struct`, types) {
				structWriter = m.Response()
			}
			if com.InSlice(`data`, types) {
				dataWriter = m.Response()
			}
		default:
			async = true
			if com.InSlice(`struct`, types) {
				structFile := filepath.Join(saveDir, m.dbName+`-struct-`+nowTime+`.sql`)
				sqlFiles = append(sqlFiles, structFile)
				structWriter = structFile
				fi := &FileInfo{
					Start: time.Now(),
					Path:  structFile,
				}
				*fileInfos = append(*fileInfos, fi)
			}
			if com.InSlice(`data`, types) {
				dataFile := filepath.Join(saveDir, m.dbName+`-data-`+nowTime+`.sql`)
				sqlFiles = append(sqlFiles, dataFile)
				dataWriter = dataFile
				fi := &FileInfo{
					Start: time.Now(),
					Path:  dataFile,
				}
				*fileInfos = append(*fileInfos, fi)
			}
		}
		cfg := *m.DbAuth
		cfg.Db = m.dbName
		worker := func(ctx context.Context) error {
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf(`%v`, r)
				}
			}()
			err = Export(ctx, &cfg, tables, structWriter, dataWriter, true)
			if err != nil {
				loga.Error(err)
				return err
			}
			if len(sqlFiles) > 0 {
				now := time.Now()
				for _, fi := range *fileInfos {
					fi.End = now
					fi.Size, err = com.FileSize(fi.Path)
					if err != nil {
						fi.Error = err.Error()
					}
					fi.Elapsed = fi.End.Sub(fi.Start)
				}
				zipFile := filepath.Join(saveDir, m.dbName+"-sql-"+nowTime+".zip")
				fi := &FileInfo{
					Start:      now,
					Path:       zipFile,
					Compressed: true,
				}
				err = archiver.Zip.Make(zipFile, sqlFiles)
				if err != nil {
					loga.Error(err)
					return err
				}
				for _, sqlFile := range sqlFiles {
					os.Remove(sqlFile)
				}
				fi.Size, err = com.FileSize(zipFile)
				if err != nil {
					fi.Error = err.Error()
				}
				fi.End = time.Now()
				fi.Elapsed = fi.End.Sub(fi.Start)
				*fileInfos = append(*fileInfos, fi)
				ioutil.WriteFile(zipFile+`.txt`, com.Str2bytes(com.Dump(fileInfos, false)), os.ModePerm)
				if _, ok := exports[cacheKey]; ok {
					delete(exports, cacheKey)
				}
			}
			return nil
		}
		if !async {
			ctx, cancel := context.WithCancel(m.StdContext())
			done := make(chan struct{})
			clientGone := m.Response().StdResponseWriter().(http.CloseNotifier).CloseNotify()
			go func() {
				for {
					select {
					case <-clientGone:
						cancel()
						return
					case <-done:
						return
					}
				}
			}()
			err = worker(ctx)
			done <- struct{}{}
			return err
		}
		data := m.Data()
		data.SetInfo(m.T(`任务已经在后台成功启动`))
		data.SetURL(handler.URLFor(`/download/file?path=dbmanager/cache/export`))
		go worker(m)
		exports[cacheKey] = fileInfos
		backgrounds.Store(OpExport, exports)
		return m.JSON(data)
	}
	return m.Redirect(m.GenURL(`listTable`, m.dbName))
}
