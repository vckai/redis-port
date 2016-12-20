// Copyright 2016 CodisLabs. All Rights Reserved.
// Licensed under the MIT (MIT-LICENSE.txt) license.

package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"github.com/CodisLabs/redis-port/pkg/libs/atomic2"
	"github.com/CodisLabs/redis-port/pkg/libs/io/pipe"
	"github.com/CodisLabs/redis-port/pkg/libs/log"
	"github.com/CodisLabs/redis-port/pkg/rdb"
)

type cmdStat struct {
	rbytes, wbytes, nentry, ignore atomic2.Int64

	forward, nbypass atomic2.Int64

	stats []*keyStat
}

type cmdStatStat struct {
	rbytes, wbytes, nentry, ignore int64

	forward, nbypass int64
}

type keyStat struct {
	key    string
	number int64
	size   int64
}

func (cmd *cmdStat) Stat() *cmdStatStat {
	return &cmdStatStat{
		rbytes: cmd.rbytes.Get(),
		wbytes: cmd.wbytes.Get(),
		nentry: cmd.nentry.Get(),
		ignore: cmd.ignore.Get(),

		forward: cmd.forward.Get(),
		nbypass: cmd.nbypass.Get(),
	}
}

func newKeyStat(key string) *keyStat {
	return &keyStat{
		key:    key,
		number: 0,
		size:   0,
	}
}

func (cmd *cmdStat) Main() {
	from := args.from
	if len(from) == 0 {
		log.Panic("invalid argument: from")
	}

	log.Infof("stat from '%s' \n", from)

	if len(args.prefix) == 0 {
		log.Panic("invalid argument: prefix")
	}

	keys := strings.Split(args.prefix, ",")
	for _, key := range keys {
		cmd.stats = append(cmd.stats, newKeyStat(key))
	}

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

	cmd.statRDBFile(reader, nsize)
}

func (cmd *cmdStat) SendSyncCmd(master, passwd string) (net.Conn, int64) {
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

func (cmd *cmdStat) SendPSyncCmd(master, passwd string) (pipe.Reader, int64) {
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

func (cmd *cmdStat) PSyncPipeCopy(c net.Conn, br *bufio.Reader, bw *bufio.Writer, offset int64, copyto io.Writer) (int64, error) {
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

func (cmd *cmdStat) statRDBFile(reader *bufio.Reader, nsize int64) {
	pipe := newRDBLoader(reader, &cmd.rbytes, args.parallel*32)
	wait := make(chan struct{})
	go func() {
		defer close(wait)
		group := make(chan int, args.parallel)
		for i := 0; i < cap(group); i++ {
			go func() {
				defer func() {
					group <- 0
				}()

				for e := range pipe {
					cmd.statRdbEntry(e)
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

	for _, stat := range cmd.stats {
		fmt.Println("key:", stat.key, ", num:", stat.number, ", size:", stat.size)
	}

	log.Info("stat rdb done")
}

func (cmd *cmdStat) statRdbEntry(e *rdb.BinEntry) {

	// 过滤key前缀
	// 统计指定前缀的keys
	for _, stat := range cmd.stats {
		if !strings.HasPrefix(string(e.Key), stat.key) {
			continue
		}

		stat.number++
		stat.size += int64(len(string(e.Key)) + len(string(e.Value)))
	}
}
