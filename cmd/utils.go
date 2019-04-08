// Copyright 2016 CodisLabs. All Rights Reserved.
// Licensed under the MIT (MIT-LICENSE.txt) license.

package main

import (
	"bufio"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/CodisLabs/redis-port/pkg/libs/atomic2"
	"github.com/CodisLabs/redis-port/pkg/libs/errors"
	"github.com/CodisLabs/redis-port/pkg/libs/log"
	"github.com/CodisLabs/redis-port/pkg/libs/stats"
	"github.com/CodisLabs/redis-port/pkg/rdb"
	"github.com/CodisLabs/redis-port/pkg/redis"
	redigo "github.com/garyburd/redigo/redis"
)

// check pkey refix exists
func inPrefixKeys(key []byte) bool {
	if len(args.preKeys) == 0 {
		return true
	}

	for _, k := range args.preKeys {
		if strings.HasPrefix(string(key), k) {
			return true
		}
	}

	return false
}

// connection redis
func openRedisConn(target, passwd string) redigo.Conn {
	return redigo.NewConn(openNetConn(target, passwd), 0, 0)
}

// connection tcp
func openNetConn(target, passwd string) net.Conn {
	c, err := net.Dial("tcp", target)
	if err != nil {
		log.PanicErrorf(err, "cannot connect to '%s'", target)
	}
	authPassword(c, passwd)
	return c
}

func openNetConnSoft(target, passwd string) net.Conn {
	c, err := net.Dial("tcp", target)
	if err != nil {
		return nil
	}
	authPassword(c, passwd)
	return c
}

func openReadFile(name string) (*os.File, int64) {
	f, err := os.Open(name)
	if err != nil {
		log.PanicErrorf(err, "cannot open file-reader '%s'", name)
	}
	s, err := f.Stat()
	if err != nil {
		log.PanicErrorf(err, "cannot stat file-reader '%s'", name)
	}
	return f, s.Size()
}

func openWriteFile(name string) *os.File {
	f, err := os.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0666)
	if err != nil {
		log.PanicErrorf(err, "cannot open file-writer '%s'", name)
	}
	return f
}

func openReadWriteFile(name string) *os.File {
	f, err := os.OpenFile(name, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0600)
	if err != nil {
		log.PanicErrorf(err, "cannot open file-readwriter '%s'", name)
	}
	return f
}

// auth redis password
func authPassword(c net.Conn, passwd string) {
	if passwd == "" {
		return
	}
	_, err := c.Write(redis.MustEncodeToBytes(redis.NewCommand("auth", passwd)))
	if err != nil {
		log.PanicError(errors.Trace(err), "write auth command failed")
	}
	var b = make([]byte, 5)
	if _, err := io.ReadFull(c, b); err != nil {
		log.PanicError(errors.Trace(err), "read auth response failed")
	}
	if strings.ToUpper(string(b)) != "+OK\r\n" {
		log.Panic("auth failed")
	}
}

func openSyncConn(target string, passwd string) (net.Conn, <-chan int64) {
	c := openNetConn(target, passwd)
	if _, err := c.Write(redis.MustEncodeToBytes(redis.NewCommand("sync"))); err != nil {
		log.PanicError(errors.Trace(err), "write sync command failed")
	}
	return c, waitRdbDump(c)
}

func waitRdbDump(r io.Reader) <-chan int64 {
	size := make(chan int64)
	go func() {
		var rsp string
		for {
			b := []byte{0}
			if _, err := r.Read(b); err != nil {
				log.PanicErrorf(err, "read sync response = '%s'", rsp)
			}
			if len(rsp) == 0 && b[0] == '\n' {
				size <- 0
				continue
			}
			rsp += string(b)
			if strings.HasSuffix(rsp, "\r\n") {
				break
			}
		}
		if rsp[0] != '$' {
			log.Panicf("invalid sync response, rsp = '%s'", rsp)
		}
		n, err := strconv.Atoi(rsp[1 : len(rsp)-2])
		if err != nil || n <= 0 {
			log.PanicErrorf(err, "invalid sync response = '%s', n = %d", rsp, n)
		}
		size <- int64(n)
	}()
	return size
}

func sendPSyncFullsync(br *bufio.Reader, bw *bufio.Writer) (string, int64, <-chan int64) {
	cmd := redis.NewCommand("psync", "?", -1)
	if err := redis.Encode(bw, cmd, true); err != nil {
		log.PanicError(err, "write psync command failed, fullsync")
	}
	r, err := redis.Decode(br)
	if err != nil {
		log.PanicError(err, "invalid psync response, fullsync")
	}
	if e, ok := r.(*redis.Error); ok {
		log.Panicf("invalid psync response, fullsync, %s", e.Value)
	}
	x, err := redis.AsString(r, nil)
	if err != nil {
		log.PanicError(err, "invalid psync response, fullsync")
	}
	xx := strings.Split(x, " ")
	if len(xx) != 3 || strings.ToLower(xx[0]) != "fullresync" {
		log.Panicf("invalid psync response = '%s', should be fullsync", x)
	}
	v, err := strconv.ParseInt(xx[2], 10, 64)
	if err != nil {
		log.PanicError(err, "parse psync offset failed")
	}
	runid, offset := xx[1], v-1
	return runid, offset, waitRdbDump(br)
}

func sendPSyncContinue(br *bufio.Reader, bw *bufio.Writer, runid string, offset int64) {
	cmd := redis.NewCommand("psync", runid, offset+2)
	if err := redis.Encode(bw, cmd, true); err != nil {
		log.PanicError(err, "write psync command failed, continue")
	}
	r, err := redis.Decode(br)
	if err != nil {
		log.PanicError(err, "invalid psync response, continue")
	}
	if e, ok := r.(*redis.Error); ok {
		log.Panicf("invalid psync response, continue, %s", e.Value)
	}
	x, err := redis.AsString(r, nil)
	if err != nil {
		log.PanicError(err, "invalid psync response, continue")
	}
	xx := strings.Split(x, " ")
	if len(xx) != 1 || strings.ToLower(xx[0]) != "continue" {
		log.Panicf("invalid psync response = '%s', should be continue", x)
	}
}

func sendPSyncAck(bw *bufio.Writer, offset int64) error {
	cmd := redis.NewCommand("replconf", "ack", offset)
	return redis.Encode(bw, cmd, true)
}

func selectDB(c redigo.Conn, db uint32) {
	s, err := redigo.String(c.Do("select", db))
	if err != nil {
		log.PanicError(err, "select command error")
	}
	if s != "OK" {
		log.Panicf("select command response = '%s', should be 'OK'", s)
	}
}

// sync redis command
func restoreRdbEntry(c redigo.Conn, e *rdb.BinEntry, pos ...int) {
	if !inPrefixKeys(e.Key) {
		return
	}

	var ttlms uint64
	if e.ExpireAt != 0 {
		now := uint64(time.Now().Add(args.shift).UnixNano())
		now /= uint64(time.Millisecond)
		if now >= e.ExpireAt {
			ttlms = 1
		} else {
			ttlms = e.ExpireAt - now
		}
	}

	// replace target redis keys
	if args.replace || args.delete {
		_, err := c.Do("del", e.Key)
		if err != nil {
			log.Println(err.Error(), " delete target key error. key:", string(e.Key))
		}

		// delete keys
		if args.delete {
			return
		}
	}

	s, err := redigo.String(c.Do("restore", e.Key, ttlms, e.Value))
	if err != nil {
		if strings.Contains(err.Error(), "Target key name") {
			log.Println(err.Error(), "skip target key name exists, key:", string(e.Key), " - ", pos[0])
			return
		}
		if strings.Contains(err.Error(), "Bad data format") {
			log.Println(err.Error(), "Bad data, key:", string(e.Key), " - ", pos[0])
			return
		}
		log.PanicError(err, "restore command error, ", string(e.Key), " - ", pos[0])
	}
	if s != "OK" {
		log.Panicf("restore command response = '%s', should be 'OK'", s)
	}
}

func iocopy(r io.Reader, w io.Writer, p []byte, max int) int {
	if max <= 0 || len(p) == 0 {
		log.Panicf("invalid max = %d, len(p) = %d", max, len(p))
	}
	if len(p) > max {
		p = p[:max]
	}
	if n, err := r.Read(p); err != nil {
		log.PanicError(err, "read error")
	} else {
		p = p[:n]
	}
	if _, err := w.Write(p); err != nil {
		log.PanicError(err, "write error")
	}
	return len(p)
}

func flushWriter(w *bufio.Writer) {
	if err := w.Flush(); err != nil {
		log.PanicError(err, "flush error")
	}
}

func newRDBLoader(reader *bufio.Reader, rbytes *atomic2.Int64, size int) chan *rdb.BinEntry {
	pipe := make(chan *rdb.BinEntry, size)
	go func() {
		defer close(pipe)
		l := rdb.NewLoader(stats.NewCountReader(reader, rbytes))
		if err := l.Header(); err != nil {
			log.PanicError(err, "parse rdb header error")
		}
		for {
			if entry, err := l.NextBinEntry(); err != nil {
				log.PanicError(err, "parse rdb entry error")
			} else {
				if entry != nil {
					pipe <- entry
				} else {
					if err := l.Footer(); err != nil {
						log.PanicError(err, "parse rdb checksum error")
					}
					return
				}
			}
		}
	}()
	return pipe
}

// find hash key on hosts for phpredis extension
func findNode(key string, hostLen int) int {
	hash := crc32(key)
	h64 := uint64(hash)
	h64 *= uint64(hostLen)
	h64 /= 0xffffffff
	pos := int(h64)

	return pos
}

// crc32
func crc32(key string) uint32 {
	table := [256]uint32{
		0x00000000, 0x77073096, 0xEE0E612C, 0x990951BA, 0x076DC419, 0x706AF48F, 0xE963A535,
		0x9E6495A3, 0x0EDB8832, 0x79DCB8A4, 0xE0D5E91E, 0x97D2D988, 0x09B64C2B, 0x7EB17CBD,
		0xE7B82D07, 0x90BF1D91, 0x1DB71064, 0x6AB020F2, 0xF3B97148, 0x84BE41DE, 0x1ADAD47D,
		0x6DDDE4EB, 0xF4D4B551, 0x83D385C7, 0x136C9856, 0x646BA8C0, 0xFD62F97A, 0x8A65C9EC,
		0x14015C4F, 0x63066CD9, 0xFA0F3D63, 0x8D080DF5, 0x3B6E20C8, 0x4C69105E, 0xD56041E4,
		0xA2677172, 0x3C03E4D1, 0x4B04D447, 0xD20D85FD, 0xA50AB56B, 0x35B5A8FA, 0x42B2986C,
		0xDBBBC9D6, 0xACBCF940, 0x32D86CE3, 0x45DF5C75, 0xDCD60DCF, 0xABD13D59, 0x26D930AC,
		0x51DE003A, 0xC8D75180, 0xBFD06116, 0x21B4F4B5, 0x56B3C423, 0xCFBA9599, 0xB8BDA50F,
		0x2802B89E, 0x5F058808, 0xC60CD9B2, 0xB10BE924, 0x2F6F7C87, 0x58684C11, 0xC1611DAB,
		0xB6662D3D, 0x76DC4190, 0x01DB7106, 0x98D220BC, 0xEFD5102A, 0x71B18589, 0x06B6B51F,
		0x9FBFE4A5, 0xE8B8D433, 0x7807C9A2, 0x0F00F934, 0x9609A88E, 0xE10E9818, 0x7F6A0DBB,
		0x086D3D2D, 0x91646C97, 0xE6635C01, 0x6B6B51F4, 0x1C6C6162, 0x856530D8, 0xF262004E,
		0x6C0695ED, 0x1B01A57B, 0x8208F4C1, 0xF50FC457, 0x65B0D9C6, 0x12B7E950, 0x8BBEB8EA,
		0xFCB9887C, 0x62DD1DDF, 0x15DA2D49, 0x8CD37CF3, 0xFBD44C65, 0x4DB26158, 0x3AB551CE,
		0xA3BC0074, 0xD4BB30E2, 0x4ADFA541, 0x3DD895D7, 0xA4D1C46D, 0xD3D6F4FB, 0x4369E96A,
		0x346ED9FC, 0xAD678846, 0xDA60B8D0, 0x44042D73, 0x33031DE5, 0xAA0A4C5F, 0xDD0D7CC9,
		0x5005713C, 0x270241AA, 0xBE0B1010, 0xC90C2086, 0x5768B525, 0x206F85B3, 0xB966D409,
		0xCE61E49F, 0x5EDEF90E, 0x29D9C998, 0xB0D09822, 0xC7D7A8B4, 0x59B33D17, 0x2EB40D81,
		0xB7BD5C3B, 0xC0BA6CAD, 0xEDB88320, 0x9ABFB3B6, 0x03B6E20C, 0x74B1D29A, 0xEAD54739,
		0x9DD277AF, 0x04DB2615, 0x73DC1683, 0xE3630B12, 0x94643B84, 0x0D6D6A3E, 0x7A6A5AA8,
		0xE40ECF0B, 0x9309FF9D, 0x0A00AE27, 0x7D079EB1, 0xF00F9344, 0x8708A3D2, 0x1E01F268,
		0x6906C2FE, 0xF762575D, 0x806567CB, 0x196C3671, 0x6E6B06E7, 0xFED41B76, 0x89D32BE0,
		0x10DA7A5A, 0x67DD4ACC, 0xF9B9DF6F, 0x8EBEEFF9, 0x17B7BE43, 0x60B08ED5, 0xD6D6A3E8,
		0xA1D1937E, 0x38D8C2C4, 0x4FDFF252, 0xD1BB67F1, 0xA6BC5767, 0x3FB506DD, 0x48B2364B,
		0xD80D2BDA, 0xAF0A1B4C, 0x36034AF6, 0x41047A60, 0xDF60EFC3, 0xA867DF55, 0x316E8EEF,
		0x4669BE79, 0xCB61B38C, 0xBC66831A, 0x256FD2A0, 0x5268E236, 0xCC0C7795, 0xBB0B4703,
		0x220216B9, 0x5505262F, 0xC5BA3BBE, 0xB2BD0B28, 0x2BB45A92, 0x5CB36A04, 0xC2D7FFA7,
		0xB5D0CF31, 0x2CD99E8B, 0x5BDEAE1D, 0x9B64C2B0, 0xEC63F226, 0x756AA39C, 0x026D930A,
		0x9C0906A9, 0xEB0E363F, 0x72076785, 0x05005713, 0x95BF4A82, 0xE2B87A14, 0x7BB12BAE,
		0x0CB61B38, 0x92D28E9B, 0xE5D5BE0D, 0x7CDCEFB7, 0x0BDBDF21, 0x86D3D2D4, 0xF1D4E242,
		0x68DDB3F8, 0x1FDA836E, 0x81BE16CD, 0xF6B9265B, 0x6FB077E1, 0x18B74777, 0x88085AE6,
		0xFF0F6A70, 0x66063BCA, 0x11010B5C, 0x8F659EFF, 0xF862AE69, 0x616BFFD3, 0x166CCF45,
		0xA00AE278, 0xD70DD2EE, 0x4E048354, 0x3903B3C2, 0xA7672661, 0xD06016F7, 0x4969474D,
		0x3E6E77DB, 0xAED16A4A, 0xD9D65ADC, 0x40DF0B66, 0x37D83BF0, 0xA9BCAE53, 0xDEBB9EC5,
		0x47B2CF7F, 0x30B5FFE9, 0xBDBDF21C, 0xCABAC28A, 0x53B39330, 0x24B4A3A6, 0xBAD03605,
		0xCDD70693, 0x54DE5729, 0x23D967BF, 0xB3667A2E, 0xC4614AB8, 0x5D681B02, 0x2A6F2B94,
		0xB40BBE37, 0xC30C8EA1, 0x5A05DF1B, 0x2D02EF8D}

	ret := uint32(0xffffffff)
	keyLen := len(key)

	for i := 0; i < keyLen; i++ {
		ret = (ret >> 8) ^ table[(ret^uint32(key[i]))&0xFF]
	}

	return (ret ^ 0xFFFFFFFF)
}
