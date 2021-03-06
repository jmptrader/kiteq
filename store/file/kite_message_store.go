package file

import (
	"container/list"
	"encoding/json"
	"errors"
	"fmt"
	log "github.com/blackbeans/log4go"
	"hash/crc32"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

//opcommand
type command struct {
	id      int64
	logicId string
	msg     []byte
	opbody  []byte
	seg     *Segment
}

func NewCommand(id int64, logicId string, msg []byte, opbody []byte) *command {
	return &command{
		id:      id,
		logicId: logicId,
		msg:     msg,
		opbody:  opbody}
}

//的快照
type MessageStore struct {
	filePath     string
	baseDir      *os.File
	segments     Segments
	chunkId      int64
	writeChannel chan *command
	running      bool
	waitSync     *sync.WaitGroup
	batchSize    int
	segcacheSize int                //segment cache size
	segmentCache *list.List         //segment cached
	replay       func(oplog *oplog) //oplog replay
	checkPeriod  time.Duration
	sync.RWMutex
}

func NewMessageStore(filePath string, batchSize int, segcacheSize int,
	checkPeriod time.Duration, replay func(oplog *oplog)) *MessageStore {

	ms := &MessageStore{
		chunkId:      -1,
		filePath:     filePath,
		segments:     make(Segments, 0, 50),
		writeChannel: make(chan *command, 10000),
		batchSize:    batchSize,
		segcacheSize: segcacheSize,
		segmentCache: list.New(),
		waitSync:     &sync.WaitGroup{},
		replay:       replay,
		checkPeriod:  checkPeriod}

	return ms
}

func (self *MessageStore) Start() {
	self.running = true
	self.load()
	go self.sync()
	go self.evict()
	self.waitSync.Add(1)
}

//
func (self *MessageStore) evict() {
	//delete segment  if all chunks are deleted

	for self.running {
		time.Sleep(self.checkPeriod)
		stat := ""
		//check cache segment
		for e := self.segmentCache.Back(); nil != e; e = e.Prev() {
			if nil != e {
				s := e.Value.(*Segment)
				if nil != s {
					//try open
					s.RLock()
					s.Open()
					total, normal, del, expired := s.stat()
					stat += fmt.Sprintf("|%s\t|%d\t|%d\t|%d\t|%d\t|\n", s.name, total, normal, del, expired)
					s.RUnlock()
					if normal <= 0 {
						//remove from cache
						self.segmentCache.Remove(e)
						//delete
						go self.remove(s)
					}
				}
			}
		}
		if len(stat) > 0 {
			log.Info("---------------MessageStore-Stat--------------\n|segment\t\t|total\t|normal\t|delete\t|expired\t|\n%s", stat)
		}
	}
}

//remove segment
func (self *MessageStore) remove(s *Segment) {
	s.Lock()
	defer s.Unlock()
	s.Close()
	os.Remove(s.path)
	os.Remove(s.slog.path)
	//backlog
	// os.Rename(s.slog.path, s.slog.path+"."+fmt.Sprintf("%d", time.Now().Unix()))

	//remove from segments
	for i, s := range self.segments {
		if s.sid == s.sid {
			self.segments = append(self.segments[0:i], self.segments[i+1:]...)
			break
		}
	}

	log.Info("MessageStore|Remove|Segment|%s", s.path)
}

func (self *MessageStore) load() {
	log.Info("MessageStore|Load Segments ...")

	if !dirExist(self.filePath) {
		err := os.MkdirAll(self.filePath, os.ModePerm)
		if nil != err {
			log.Error("MessageStore|Load Segments|MKDIR|FAIL|%s|%s", err, self.filePath)
			panic(err)
		}
	}

	bashDir, err := os.Open(self.filePath)
	if nil != err {
		log.Error("MessageStore|Load Segments|FAIL|%s|%s", err, self.filePath)
		panic(err)
	}

	self.baseDir = bashDir

	//segment builder
	segmentBuilder := func(path string, f os.FileInfo) *Segment {

		logpath := strings.SplitN(path, ".", 2)[0] + SEGMENT_LOG_SUFFIX

		name := strings.TrimSuffix(f.Name(), SEGMENT_DATA_SUFFIX)
		split := strings.SplitN(name, "-", 2)
		sid := int64(0)
		if len(split) >= 2 {
			id, err := strconv.ParseInt(split[1], 10, 64)
			if nil != err {
				log.Error("MessageStore|Load Segments|Parse SegmentId|FAIL|%s|%s", err, name)
				return nil
			}
			sid = id
		}

		//segment log
		sl := newSegmentLog(logpath)
		// create segment
		return newSegment(path, f.Name(), sid, sl)
	}

	//fetch all Segment
	filepath.Walk(self.filePath, func(path string, f os.FileInfo, err error) error {
		// log.Info("MessageStore|Walk|%s", path)
		if nil != f && !f.IsDir() {
			split := strings.SplitN(f.Name(), ".", 2)
			suffix := split[1]
			//is data file or logfile

			if strings.HasSuffix(SEGMENT_DATA_SUFFIX, suffix) {
				seg := segmentBuilder(path, f)
				self.segments = append(self.segments, seg)
				// log.Info("MessageStore|load|segmentBuilder|%s", path)
			} else if strings.HasSuffix(SEGMENT_LOG_SUFFIX, suffix) {
				//log suffix
				return nil
			}
		}

		return nil
	})

	//sort segments
	sort.Sort(self.segments)

	//recover snapshost
	self.recoverSnapshot()

	//check roll
	self.checkRoll()

	//load fixed num  segments into memory

	log.Info("MessageStore|Load|SUCC|%s", self)
}

func (self *MessageStore) recoverSnapshot() {
	//current segmentid
	if len(self.segments) > 0 {

		//replay log
		for i, s := range self.segments {
			err := s.Open()
			if nil != err {
				log.Error("MessageStore|recoverSnapshot|Fail|%s", err, s.slog.path)
				panic(err)
			}

			//last segments
			if i == len(self.segments)-1 {
				if nil != err {
					panic("MessageStore|Load Last Segment|FAIL|" + err.Error())
				}

				//set snapshost status
				if len(s.chunks) > 0 {
					self.chunkId = s.chunks[len(s.chunks)-1].id
				}
			}
			//replay segment log
			s.slog.Replay(self.replay)

		}

	}
}

//query one chunk by  chunkid
func (self *MessageStore) Query(cid int64, ins interface{}) error {

	curr := self.indexSegment(cid)
	if nil == curr {
		return errors.New(fmt.Sprintf("No Segement For %d", cid))
	}

	curr.RLock()
	defer curr.RUnlock()
	//find chunk
	c := curr.Get(cid)
	if nil != c {
		// log.Debug("MessageStore|QUERY|%s|%t", curr.name, c)
		err := json.Unmarshal(c.data, ins)
		if nil != err {
			return err
		}
		return nil
	}
	// log.Debug("MessageStore|QUERY|%s", curr.name)
	return errors.New(fmt.Sprintf("No Chunk For [%s,%d]", curr.name, cid))
}

//index segment
func (self *MessageStore) indexSegment(cid int64) *Segment {
	var curr *Segment
	self.RLock()
	//check cid in cache
	for e := self.segmentCache.Front(); nil != e; e = e.Next() {
		s := e.Value.(*Segment)
		if s.sid <= cid && cid < s.sid+int64(len(s.chunks)) {
			curr = s
			break
		}
	}
	self.RUnlock()

	// not exist In cache
	if nil == curr {
		self.Lock()
		idx := sort.Search(len(self.segments), func(i int) bool {
			s := self.segments[i]
			return s.sid >= cid
		})

		if idx >= len(self.segments) || self.segments[idx].sid != cid {
			idx = idx - 1
		}

		//load segment
		self.loadSegment(idx)
		curr = self.segments[idx]
		// log.Debug("MessageStore|indexSegment|%d", curr.path)
		self.Unlock()

	}
	return curr
}

//return the front chunk
func (self *MessageStore) loadSegment(idx int) {

	// load n segments
	s := self.segments[idx]
	err := s.Open()
	if nil != err {
		log.Error("MessageStore|loadSegment|FAIL|%s|%s\n", err, s.name)
		return
	} else {
		exsit := false
		//pop header
		for e := self.segmentCache.Back(); nil != e; e = e.Prev() {
			id := e.Value.(*Segment).sid
			if self.segmentCache.Len() >= self.segcacheSize && id != s.sid {
				self.segmentCache.Remove(e)
			} else if id == s.sid {
				exsit = true
			}

		}

		if !exsit {
			//push to cache
			self.segmentCache.PushFront(s)
			log.Info("MessageStore|loadSegment|SUCC|%s|cache-Len:%d", s.name, self.segmentCache.Len())
		}
	}
	// log.Info("MessageStore|loadSegment|SUCC|%s", s.name)
}

//append log
func (self *MessageStore) Update(c *command) {
	s := self.indexSegment(c.id)
	if nil != s {
		s.Lock()
		defer s.Unlock()
		//append oplog
		ol := newOplog(OP_U, c.logicId, c.id, c.opbody)
		s.slog.Append(ol)
	}
}

//mark delete
func (self *MessageStore) Delete(c *command) {
	s := self.indexSegment(c.id)
	if nil != s {
		s.Lock()
		defer s.Unlock()
		//append oplog
		ol := newOplog(OP_D, c.logicId, c.id, c.opbody)
		s.slog.Append(ol)
		//mark data delete
		s.Delete(c.id)

	} else {
		// log.Debug("MessageStore|Delete|chunkid:%d|%s\n", cid, s)
	}
}

//mark data expired
func (self *MessageStore) Expired(c *command) {
	s := self.indexSegment(c.id)
	if nil != s {
		s.Lock()
		defer s.Unlock()
		//append oplog logic delete
		ol := newOplog(OP_E, c.logicId, c.id, c.opbody)
		s.slog.Append(ol)
		//mark data expired
		s.Expired(c.id)

	} else {
		// log.Debug("MessageStore|Expired|chunkid:%d|%s\n", cid, s)
	}
}

//write
func (self *MessageStore) Append(cmd *command) int64 {

	if self.running {
		self.Lock()
		defer self.Unlock()
		cmd.id = self.cid()
		seg := self.checkRoll()
		cmd.seg = seg
		//append log
		ol := newOplog(OP_C, cmd.logicId, cmd.id, cmd.opbody)
		seg.Lock()
		err := seg.slog.Append(ol)
		seg.Unlock()
		if nil != err {
			log.Error("MessageStore|Append-LOG|FAIL|%s", cmd)
			return -1
		}

		//write to channel for async flush
		self.writeChannel <- cmd
		return cmd.id
	} else {
		return -1
	}

}

func flush(s *Segment, b []*Chunk) {
	if len(b) > 0 {
		s.Lock()
		//complete
		err := s.Append(b)
		if nil != err {
			log.Error("MessageStore|Append|FAIL|%s\n", err)
		}

		s.Unlock()
	}
}

func (self *MessageStore) sync() {

	batch := make([]*Chunk, 0, self.batchSize)

	var cmd *command
	ticker := time.NewTicker(500 * time.Millisecond)
	var curr *Segment
	for self.running {

		//no batch / wait for data
		select {
		case cmd = <-self.writeChannel:
		case <-ticker.C:
			//no write data flush
		}

		if nil != cmd {
			c := cmd
			//init curr segment
			if nil == curr {
				curr = cmd.seg
			}

			//check if segment changed,and then flush data
			//else flush old data
			if curr.sid != c.seg.sid {
				//force flush
				flush(curr, batch)
				batch = batch[:0]
				//change curr to  newsegment
				curr = c.seg
			}

			//create chunk
			chunk := &Chunk{
				length:   int32(CHUNK_HEADER + len(c.msg)),
				id:       c.id,
				checksum: crc32.ChecksumIEEE(c.msg),
				data:     c.msg,
				flag:     NORMAL}
			batch = append(batch, chunk)
		}

		//force flush
		if nil == cmd && len(batch) > 0 || len(batch) >= cap(batch) {
			flush(curr, batch)
			batch = batch[:0]
		}
		cmd = nil
	}

	// need flush left data
outter:
	for {
		select {
		case c := <-self.writeChannel:
			if nil != c {
				if c.seg.sid != curr.sid {
					flush(curr, batch)
					batch = batch[:0]
				}

				//create chunk
				chunk := &Chunk{
					length:   int32(CHUNK_HEADER + len(c.msg)),
					id:       c.id,
					checksum: crc32.ChecksumIEEE(c.msg),
					data:     c.msg,
					flag:     NORMAL}
				batch = append(batch, chunk)
			} else {
				//channel close
				break outter
			}

		default:
			break outter
		}
	}

	//last flush
	flush(curr, batch)

	ticker.Stop()
	self.waitSync.Done()
	log.Info("MessageStore|SYNC|CLOSE...")
}

//check if
func (self *MessageStore) checkRoll() *Segment {
	//if current segment bytesize is larger than max segment size
	//create a new segment for storage

	var s *Segment
	if len(self.segments) <= 0 {
		news, err := self.createSegment(0)
		if nil == err {
			self.Lock()
			//append new
			self.segments = append(self.segments, news)

			self.Unlock()
			s = news

		} else {
			//panic  first segment fail
			panic(err)
		}
	} else {
		self.Lock()
		s = self.segments[len(self.segments)-1]
		if s.byteSize > MAX_SEGMENT_SIZE {
			nid := s.chunks[len(s.chunks)-1].id + 1

			news, err := self.createSegment(nid)
			if nil == err {
				//left segments are larger than cached ,close current
				if len(self.segments) >= self.segcacheSize {
					s.Close()
				}
				//append new
				self.segments = append(self.segments, news)
				s = news
			}

		}
		self.Unlock()
	}
	return s
}

//create segemnt
func (self *MessageStore) createSegment(nextStart int64) (*Segment, error) {
	name := fmt.Sprintf("%s-%d", SEGMENT_PREFIX, nextStart) + SEGMENT_DATA_SUFFIX
	logname := fmt.Sprintf("%s-%d", SEGMENT_PREFIX, nextStart) + SEGMENT_LOG_SUFFIX

	sl := &SegmentLog{
		offset: 0,
		path:   self.filePath + logname}

	news := newSegment(self.filePath+name, name, nextStart, sl)

	err := news.Open()
	if nil != err {
		log.Error("MessageStore|currentSegment|Open Segment|FAIL|%s", news.path)
		return nil, err
	}
	return news, nil

}

//remove sid
func (self *MessageStore) Remove(sid int64) {

	//check cid in cache
	for e := self.segmentCache.Front(); nil != e; e = e.Next() {
		s := e.Value.(*Segment)
		s.RLock()
		if s.sid == sid {
			self.segmentCache.Remove(e)
			s.Close()
			os.Remove(s.path)

			//remove from segments
			for i, s := range self.segments {
				if s.sid == s.sid {
					self.segments = append(self.segments[0:i], self.segments[i+1:]...)
					break
				}
			}

			log.Info("MessageStore|Remove|Segment|%s", s.path)
			break
		}
		s.RUnlock()
	}
}

func (self *MessageStore) Destory() {
	self.running = false
	close(self.writeChannel)
	self.waitSync.Wait()
	//close all segment
	for _, s := range self.segments {
		err := s.Close()
		if nil != err {
			log.Error("MessageStore|Destory|Close|FAIL|%s|sid:%d", err, s.sid)
		}
	}
	self.baseDir.Close()
	log.Info("MessageStore|Destory...")

}

//chunk id
func (self *MessageStore) cid() int64 {
	return atomic.AddInt64(&self.chunkId, 1)
}

func (self MessageStore) String() string {
	return fmt.Sprintf("filePath:%s\tchunkid:%d\tsegments:%d",
		self.filePath, self.chunkId, len(self.segments))
}

// 检查目录是否存在
func dirExist(dir string) bool {
	info, err := os.Stat(dir)
	if err == nil {
		return info.IsDir()
	} else {
		return !os.IsNotExist(err) && info.IsDir()
	}
}
