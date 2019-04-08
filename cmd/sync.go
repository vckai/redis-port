// Copyright 2016 CodisLabs. All Rights Reserved.
// Licensed under the MIT (MIT-LICENSE.txt) license.

package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"os"
	"strings"
	"time"

	"github.com/CodisLabs/redis-port/pkg/libs/atomic2"
	"github.com/CodisLabs/redis-port/pkg/libs/io/pipe"
	"github.com/CodisLabs/redis-port/pkg/libs/log"
	"github.com/CodisLabs/redis-port/pkg/libs/stats"
	"github.com/CodisLabs/redis-port/pkg/redis"
	redigo "github.com/garyburd/redigo/redis"
)

type cmdSync struct {
	rbytes, wbytes, nentry, ignore atomic2.Int64

	forward, nbypass atomic2.Int64

	preKeys map[string]bool
}

type cmdSyncStat struct {
	rbytes, wbytes, nentry, ignore int64

	forward, nbypass int64
}

func (cmd *cmdSync) Stat() *cmdSyncStat {
	return &cmdSyncStat{
		rbytes: cmd.rbytes.Get(),
		wbytes: cmd.wbytes.Get(),
		nentry: cmd.nentry.Get(),
		ignore: cmd.ignore.Get(),

		forward: cmd.forward.Get(),
		nbypass: cmd.nbypass.Get(),
	}
}

func (cmd *cmdSync) Main() {
	from, target := args.from, args.target
	if len(from) == 0 {
		log.Panic("invalid argument: from")
	}
	if len(target) == 0 {
		log.Panic("invalid argument: target")
	}

	log.Infof("sync from '%s' to '%s'\n", from, target)

	var sockfile *os.File
	if len(args.sockfile) != 0 {
		sockfile = openReadWriteFile(args.sockfile)
		defer sockfile.Close()
	}

	var input io.ReadCloser
	var nsize int64
	if args.psync {
		input, nsize = cmd.SendPSyncCmd(from, args.passwd)
	} else {
		input, nsize = cmd.SendSyncCmd(from, args.passwd)
	}
	defer input.Close()

	log.Infof("rdb file = %d\n", nsize)

	if sockfile != nil {
		r, w := pipe.NewFilePipe(int(args.filesize), sockfile)
		defer r.Close()
		go func(r io.Reader) {
			defer w.Close()
			p := make([]byte, ReaderBufferSize)
			for {
				iocopy(r, w, p, len(p))
			}
		}(input)
		input = r
	}

	reader := bufio.NewReaderSize(input, ReaderBufferSize)

	cmd.SyncRDBFile(reader, target, args.auth, nsize)

	// 删除操作不支持增量执行
	if !args.delete {
		cmd.SyncCommand(reader, target, args.auth)
	}
}

func (cmd *cmdSync) SendSyncCmd(master, passwd string) (net.Conn, int64) {
	c, wait := openSyncConn(master, passwd)
	for {
		select {
		case nsize := <-wait:
			if nsize == 0 {
				log.Info("+")
			} else {
				return c, nsize
			}
		case <-time.After(time.Second):
			log.Info("-")
		}
	}
}

func (cmd *cmdSync) SendPSyncCmd(master, passwd string) (pipe.Reader, int64) {
	c := openNetConn(master, passwd)
	br := bufio.NewReaderSize(c, ReaderBufferSize)
	bw := bufio.NewWriterSize(c, WriterBufferSize)

	runid, offset, wait := sendPSyncFullsync(br, bw)
	log.Infof("psync runid = %s offset = %d, fullsync", runid, offset)

	var nsize int64
	for nsize == 0 {
		select {
		case nsize = <-wait:
			if nsize == 0 {
				log.Info("+")
			}
		case <-time.After(time.Second):
			log.Info("-")
		}
	}

	piper, pipew := pipe.NewSize(ReaderBufferSize)

	go func() {
		defer pipew.Close()
		p := make([]byte, 8192)
		for rdbsize := int(nsize); rdbsize != 0; {
			rdbsize -= iocopy(br, pipew, p, rdbsize)
		}
		for {
			n, err := cmd.PSyncPipeCopy(c, br, bw, offset, pipew)
			if err != nil {
				log.PanicErrorf(err, "psync runid = %s, offset = %d, pipe is broken", runid, offset)
			}
			offset += n
			for {
				time.Sleep(time.Second)
				c = openNetConnSoft(master, passwd)
				if c != nil {
					log.Infof("psync reopen connection, offset = %d", offset)
					break
				} else {
					log.Infof("psync reopen connection, failed")
				}
			}
			authPassword(c, passwd)
			br = bufio.NewReaderSize(c, ReaderBufferSize)
			bw = bufio.NewWriterSize(c, WriterBufferSize)
			sendPSyncContinue(br, bw, runid, offset)
		}
	}()
	return piper, nsize
}

func (cmd *cmdSync) PSyncPipeCopy(c net.Conn, br *bufio.Reader, bw *bufio.Writer, offset int64, copyto io.Writer) (int64, error) {
	defer c.Close()
	var nread atomic2.Int64
	go func() {
		defer c.Close()
		for {
			time.Sleep(time.Second * 5)
			if err := sendPSyncAck(bw, offset+nread.Get()); err != nil {
				return
			}
		}
	}()

	var p = make([]byte, 8192)
	for {
		n, err := br.Read(p)
		if err != nil {
			return nread.Get(), nil
		}
		if _, err := copyto.Write(p[:n]); err != nil {
			return nread.Get(), err
		}
		nread.Add(int64(n))
	}
}

// 全量同步redis命令
func (cmd *cmdSync) SyncRDBFile(reader *bufio.Reader, target, passwd string, nsize int64) {
	pipe := newRDBLoader(reader, &cmd.rbytes, args.parallel*32)
	wait := make(chan struct{})

	go func() {
		defer close(wait)
		group := make(chan int, 100)
		for i := 0; i < cap(group); i++ {
			go func() {
				defer func() {
					group <- 0
				}()

				// 实例化建立多个实例
				targets := strings.Split(target, ",")
				var redisConns []redigo.Conn
				for _, _target := range targets {
					c := openRedisConn(_target, passwd)
					defer c.Close()
					redisConns = append(redisConns, c)
				}
				redisLen := len(redisConns)

				var lastdb uint32 = 0
				for e := range pipe {
					if !acceptDB(e.DB) {
						cmd.ignore.Incr()
					} else {
						// 根据phpredis hash算法计算key所在的redis实例
						pos := findNode(string(e.Key), redisLen)
						c := redisConns[pos]
						cmd.nentry.Incr()
						if e.DB != lastdb {
							lastdb = e.DB
							selectDB(c, lastdb)
						}
						restoreRdbEntry(c, e, pos)
					}
				}
			}()
		}
		for i := 0; i < cap(group); i++ {
			<-group
		}
	}()

	for done := false; !done; {
		select {
		case <-wait:
			done = true
		case <-time.After(time.Second):
		}
		stat := cmd.Stat()
		var b bytes.Buffer
		fmt.Fprintf(&b, "total=%d - %12d [%3d%%]", nsize, stat.rbytes, 100*stat.rbytes/nsize)
		fmt.Fprintf(&b, "  entry=%-12d", stat.nentry)
		if stat.ignore != 0 {
			fmt.Fprintf(&b, "  ignore=%-12d", stat.ignore)
		}
		log.Info(b.String())
	}
	log.Info("sync rdb done")
}

// 增量同步命令
func (cmd *cmdSync) SyncCommand(reader *bufio.Reader, target, passwd string) {
	// 实例化建立多个实例
	targets := strings.Split(target, ",")
	var writers []*bufio.Writer
	for _, _target := range targets {
		c := openNetConn(_target, passwd)
		defer c.Close()
		writer := bufio.NewWriterSize(stats.NewCountWriter(c, &cmd.wbytes), WriterBufferSize)
		defer flushWriter(writer)

		writers = append(writers, writer)

		go func() {
			p := make([]byte, ReaderBufferSize)
			for {
				iocopy(c, ioutil.Discard, p, len(p))
			}
		}()
	}

	writerLen := len(writers)

	go func() {
		var bypass bool = false
		for {
			var writer *bufio.Writer

			resp := redis.MustDecode(reader)
			if scmd, keys, err := redis.ParseArgs(resp); err != nil {
				log.PanicError(err, "parse command arguments failed")
			} else if scmd != "ping" {
				key := string(keys[0])
				if scmd == "select" {
					if len(keys) != 1 {
						log.Panicf("select command len(args) = %d", len(keys))
					}
					n, err := parseInt(key, MinDB, MaxDB)
					if err != nil {
						log.PanicErrorf(err, "parse db = %s failed", key)
					}
					bypass = !acceptDB(uint32(n))
				}
				// 过滤key前缀
				// 迁移指定前缀的keys
				if bypass || !inPrefixKeys(keys[0]) {
					cmd.nbypass.Incr()
					continue
				}

				// 查找hash redis实例
				pos := findNode(key, writerLen)
				writer = writers[pos]
			}

			cmd.forward.Incr()
			if writer == nil {
				for _, wri := range writers {
					redis.MustEncode(wri, resp)
					flushWriter(wri)
				}
			} else {
				redis.MustEncode(writer, resp)
				flushWriter(writer)
			}
		}
	}()

	for lstat := cmd.Stat(); ; {
		time.Sleep(time.Second)
		nstat := cmd.Stat()
		var b bytes.Buffer
		fmt.Fprintf(&b, "syncCommand: ")
		fmt.Fprintf(&b, " +forward=%-6d", nstat.forward-lstat.forward)
		fmt.Fprintf(&b, " +nbypass=%-6d", nstat.nbypass-lstat.nbypass)
		fmt.Fprintf(&b, " +nbytes=%d", nstat.wbytes-lstat.wbytes)
		log.Info(b.String())
		lstat = nstat
	}
}
