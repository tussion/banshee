// Copyright 2016 Eleme Inc. All rights reserved.

package metricdb

import (
	"github.com/eleme/banshee/models"
	"github.com/eleme/banshee/util"
	"github.com/eleme/banshee/util/htree"
	"github.com/eleme/banshee/util/log"
	"github.com/eleme/banshee/util/skiplist"
	"math/rand"
	"sort"
	"sync"
	"sync/atomic"
)

// Memory structure
//
//	memStorage -+- 16931 (htree) -+- m1 (skiplist)
//	            |- 16932          |- ....
//	            |- ...
//	            |- 16939
//

// slMaxLevel is the skiplist max level.
const slMaxLevel = 32

// node is the htree node.
type node struct {
	link uint32
	sl   *skiplist.SkipList
}

// Key implements interface htree.Item.
func (n *node) Key() uint32 { return n.link }

// metricWrapper is a metric wrapper.
type metricWrapper struct{ m *models.Metric }

// Less implements interface skiplist.Item.
func (w *metricWrapper) Less(than skiplist.Item) bool {
	return w.m.Stamp < than.(*metricWrapper).m.Stamp
}

// memStorage is the memory based storage.
type memStorage struct {
	id    uint32
	htree *htree.HTree
}

// newMemStorage creates a new memStorage.
func newMemStorage(id uint32) *memStorage {
	return &memStorage{id, htree.New()}
}

// has returns true if given link is in the mem storage.
func (s *memStorage) has(link uint32) bool {
	return s.htree.Has(&node{link: link})
}

// put a metric into mem storage.
func (s *memStorage) put(m *models.Metric) error {
	if m.Link == 0 {
		return ErrNoLink
	}
	item := s.htree.Put(&node{link: m.Link})
	n := item.(*node)
	if n.sl == nil {
		n.sl = skiplist.New(slMaxLevel)
	}
	n.sl.Putnx(&metricWrapper{m})
	return nil
}

// get metrics in a stamp range, the range is left open and right closed.
func (s *memStorage) get(link, start, end uint32) (ms []*models.Metric) {
	item := s.htree.Get(&node{link: link})
	if item == nil {
		return
	}
	n := item.(*node)
	iter := n.sl.NewIterator(&metricWrapper{&models.Metric{Stamp: start}})
	for iter.Next() {
		item := iter.Item()
		w := item.(*metricWrapper)
		if w.m.Stamp >= end {
			break
		}
		ms = append(ms, w.m)
	}
	return
}

// memStoragesById implements sort.Interface for a slice of memStorages.
type memStoragesByID []*memStorage

func (b memStoragesByID) Len() int           { return len(b) }
func (b memStoragesByID) Less(i, j int) bool { return b[i].id < b[j].id }
func (b memStoragesByID) Swap(i, j int)      { b[i], b[j] = b[j], b[i] }

// memStoragePool is the memory storage pool.
type memStoragePool struct {
	opts     *Options
	pool     []*memStorage
	initOK   int32
	initErr  int32
	initCost float64      // stat
	lock     sync.RWMutex // protects the pool
}

// newMemStoragePool creates a new memStoragePool.
func newMemStoragePool(opts *Options) *memStoragePool {
	return &memStoragePool{opts: opts, initOK: 0, initErr: 0}
}

// isInitOK returns true if the init ok.
func (p *memStoragePool) isInitOK() bool {
	return atomic.LoadInt32(&p.initOK) == 1
}

// isInitErr returns true if the init erorred.
func (p *memStoragePool) isInitErr() bool {
	return atomic.LoadInt32(&p.initErr) == 1
}

// create a mem storage for given stamp.
func (p *memStoragePool) create(stamp uint32, force bool) {
	id := stamp / p.opts.Period
	if !force {
		if len(p.pool) > 0 && id <= p.pool[len(p.pool)-1].id {
			return
		}
		p.pool = append(p.pool, newMemStorage(id))
		log.Infof("mem storage %d created", id)
		return
	}
	for _, s := range p.pool {
		if s.id == id { // Distinct
			return
		}
	}
	p.pool = append(p.pool, newMemStorage(id))
	sort.Sort(memStoragesByID(p.pool))
	log.Infof("mem storage %d created forcely", id)
	return
}

// expire oudated mem storages.
func (p *memStoragePool) expire() {
	if len(p.pool) == 0 {
		return
	}
	id := p.pool[len(p.pool)-1].id - p.opts.Expiration/p.opts.Period
	for i, s := range p.pool {
		if s.id < id {
			s.htree.Clear() // gc
			p.pool = p.pool[i+1:]
			log.Infof("mem storage %d expired", s.id)
		}
	}
}

// adjust the pool.
func (p *memStoragePool) adjust(stamp uint32, force bool) {
	p.create(stamp, force)
	p.expire()
}

// put0 is the put without adjust.
func (p *memStoragePool) put0(m *models.Metric) (err error) {
	if len(p.pool) == 0 {
		return ErrNoMemStorage
	}
	for i := len(p.pool) - 1; i >= 0; i-- {
		s := p.pool[i]
		if s.id*p.opts.Period <= m.Stamp && m.Stamp < (s.id+1)*p.opts.Period {
			return s.put(m)
		}
	}
	return
}

// put a metric into pool.
func (p *memStoragePool) put(m *models.Metric) (err error) {
	p.lock.Lock()
	defer p.lock.Unlock()
	p.adjust(m.Stamp, false)
	return p.put0(m)
}

// putf is the put with adjust force=true.
func (p *memStoragePool) putf(m *models.Metric) (err error) {
	p.lock.Lock()
	defer p.lock.Unlock()
	p.adjust(m.Stamp, true)
	return p.put0(m)
}

// get metrics in a stamp range, the range is left open and right closed.
func (p *memStoragePool) get(link, start, end uint32) (ms []*models.Metric) {
	p.lock.RLock()
	defer p.lock.RUnlock()
	if len(p.pool) == 0 {
		return
	}
	for _, s := range p.pool {
		min := s.id * p.opts.Period
		max := (s.id + 1) * p.opts.Period
		if start >= max || end < min {
			continue
		}
		st, ed := start, end
		if start < min {
			st = min
		}
		if end > max {
			ed = max
		}
		ms = append(ms, s.get(link, st, ed)...)
	}
	return
}

// has returns true if given link is in the mem storage pool.
func (p *memStoragePool) has(link uint32) bool {
	p.lock.RLock()
	defer p.lock.RUnlock()
	for _, s := range p.pool {
		if s.has(link) {
			return true
		}
	}
	return false
}

// getInitCost returns the init cost.
func (p *memStoragePool) getInitCost() float64 {
	p.lock.RLock()
	defer p.lock.RUnlock()
	return p.initCost
}

// setInitCost sets the init cost.
func (p *memStoragePool) setInitCost(n float64) {
	p.lock.Lock()
	defer p.lock.Unlock()
	p.initCost = n
}

// init all mem storages.
func (p *memStoragePool) init(fp *fileStoragePool, idxs []*models.Index) (err error) {
	var wg sync.WaitGroup
	timer := util.NewTimer()
	fp.lock.RLock()
	for _, fs := range fp.pool {
		wg.Add(1)
		go func(s *fileStorage) {
			defer wg.Done()
			for _, idx := range idxs {
				if !s.active() {
					continue
				}
				if p.has(idx.Link) || rand.Float64() < p.opts.CachePercentage {
					var ms []*models.Metric
					if ms, err = s.get(idx.Name, idx.Link, 0, idx.Stamp+1); err != nil {
						atomic.StoreInt32(&p.initErr, 1)
						log.Errorf("mem storage init fail: %v", err)
						return
					}
					for _, m := range ms {
						if err = p.putf(m); err != nil {
							atomic.StoreInt32(&p.initErr, 1)
							log.Errorf("mem storage init fail: %v", err)
							return
						}
					}
				}
			}
		}(fs)
	}
	fp.lock.RUnlock()
	wg.Wait() // Wait the init complete.
	atomic.StoreInt32(&p.initOK, 1)
	p.setInitCost(timer.Elapsed())
	log.Infof("mem storage pool init done")
	return
}
