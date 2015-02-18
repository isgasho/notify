// Copyright (c) 2014-2015 The Notify Authors. All rights reserved.
// Use of this source code is governed by the MIT license that can be
// found in the LICENSE file.

// +build linux

package notify

import (
	"bytes"
	"errors"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"
)

// eventBufferSize defines the size of the buffer given to read(2) function. One
// should not depend on this value, since it was arbitrary chosen and may be
// changed in the future.
const eventBufferSize = 64 * (syscall.SizeofInotifyEvent + syscall.PathMax + 1)

// consumersCount defines the number of consumers in producer-consumer based
// implementation. Each consumer is run in a separate goroutine and has read
// access to watched files map.
const consumersCount = 2

const invalidDescriptor = -1

// watched is a pair of file path and inotify mask used as a value in
// watched files map.
type watched struct {
	path string
	mask uint32
}

// inotify implements Watcher interface.
type inotify struct {
	sync.RWMutex
	m      map[int32]*watched
	fd     int32
	pipefd []int
	epfd   int
	epes   []syscall.EpollEvent
	buffer [eventBufferSize]byte
	wg     sync.WaitGroup
	c      chan<- EventInfo
}

// NewWatcher creates new non-recursive inotify backed by inotify.
func newWatcher(c chan<- EventInfo) *inotify {
	i := &inotify{
		m:      make(map[int32]*watched),
		fd:     invalidDescriptor,
		pipefd: []int{invalidDescriptor, invalidDescriptor},
		epfd:   invalidDescriptor,
		epes:   make([]syscall.EpollEvent, 0),
		c:      c,
	}
	runtime.SetFinalizer(i, func(i *inotify) {
		i.epollclose()
		if i.fd != invalidDescriptor {
			syscall.Close(int(i.fd))
		}
	})
	return i
}

// Watch implements notify.watcher interface.
func (i *inotify) Watch(path string, e Event) error {
	return i.watch(path, e)
}

// Rewatch implements notify.watcher interface.
func (i *inotify) Rewatch(path string, _, newevent Event) error {
	return i.watch(path, newevent)
}

// TODO : pknap
func (i *inotify) watch(path string, e Event) (err error) {
	if e&^(All|Event(syscall.IN_ALL_EVENTS)) != 0 {
		return errors.New("notify: unknown event")
	}
	if err = i.lazyinit(); err != nil {
		return
	}
	iwd, err := syscall.InotifyAddWatch(int(i.fd), path, encode(e))
	if err != nil {
		return
	}
	i.RLock()
	wd := i.m[int32(iwd)]
	i.RUnlock()
	if wd == nil {
		i.Lock()
		if i.m[int32(iwd)] == nil {
			i.m[int32(iwd)] = &watched{path: path, mask: uint32(e)}
		}
		i.Unlock()
	} else {
		i.Lock()
		wd.mask = uint32(e)
		i.Unlock()
	}
	return nil
}

// lazyinit sets up all required file descriptors and starts 1+consumersCount
// goroutines. The producer goroutine blocks until file-system notifications
// occur. Then, all events are read from system buffer and sent to consumer
// goroutines which construct valid notify events. This method uses
// Double-Checked Locking optimization.
func (i *inotify) lazyinit() error {
	if atomic.LoadInt32(&i.fd) == invalidDescriptor {
		i.Lock()
		defer i.Unlock()
		if atomic.LoadInt32(&i.fd) == invalidDescriptor {
			fd, err := syscall.InotifyInit()
			if err != nil {
				return err
			}
			i.fd = int32(fd)
			if err = i.epollinit(); err != nil {
				_, _ = i.epollclose(), syscall.Close(int(fd)) // Ignore errors.
				i.fd = invalidDescriptor
				return err
			}
			esch := make(chan []*event)
			go i.loop(esch)
			i.wg.Add(consumersCount)
			for n := 0; n < consumersCount; n++ {
				go i.send(esch)
			}
		}
	}
	return nil
}

// epollinit opens an epoll file descriptor and creates a pipe which will be
// used to wake up the epoll_wait(2) function. Then, file descriptor associated
// with inotify event queue and the read end of the pipe are added to epoll set.
// Note that `fd` member must be set before this function is called.
func (i *inotify) epollinit() (err error) {
	if i.epfd, err = syscall.EpollCreate(2); err != nil {
		return
	}
	if err = syscall.Pipe(i.pipefd); err != nil {
		return
	}
	i.epes = []syscall.EpollEvent{
		{ // inotify file descriptor.
			Events: syscall.EPOLLIN,
			Fd:     i.fd,
		},
		{ // read end of the pipe used to stop event loop.
			Events: syscall.EPOLLIN,
			Fd:     int32(i.pipefd[0]),
		},
	}
	if err = syscall.EpollCtl(i.epfd, syscall.EPOLL_CTL_ADD, int(i.fd), &i.epes[0]); err != nil {
		return
	}
	return syscall.EpollCtl(i.epfd, syscall.EPOLL_CTL_ADD, i.pipefd[0], &i.epes[1])
}

// epollclose closes the file descriptor created by the call to epoll_create(2)
// and two file descriptors opened by pipe(2) function.
func (i *inotify) epollclose() (err error) {
	if i.epfd != invalidDescriptor {
		if err = syscall.Close(i.epfd); err == nil {
			i.epfd = invalidDescriptor
		}
	}
	for n, fd := range i.pipefd {
		if fd != invalidDescriptor {
			switch e := syscall.Close(fd); {
			case e != nil && err == nil:
				err = e
			case e == nil:
				i.pipefd[n] = invalidDescriptor
			}
		}
	}
	return
}

// loop blocks until either inotify or pipe file descriptor is ready for I/O.
// All read operations triggered by filesystem notifications are forwarded to
// one of the event's consumers. If pipe fd became ready, loop function closes
// all file descriptors opened by lazyinit method and returns afterwards.
func (i *inotify) loop(esch chan<- []*event) {
	epes := make([]syscall.EpollEvent, 1)
	fd := atomic.LoadInt32(&i.fd)
	for {
		switch _, err := syscall.EpollWait(i.epfd, epes, -1); err {
		case nil:
			switch epes[0].Fd {
			case fd:
				esch <- i.read()
			case int32(i.pipefd[0]):
				i.Lock()
				defer i.Unlock()
				if err = syscall.Close(int(fd)); err != nil && err != syscall.EINTR {
					panic("notify: close(2) error - " + err.Error())
				}
				atomic.StoreInt32(&i.fd, invalidDescriptor)
				if err = i.epollclose(); err != nil && err != syscall.EINTR {
					panic("notify: epollclose error - " + err.Error())
				}
				close(esch)
				return
			}
		case syscall.EINTR:
			continue
		default: // We should never reach this line.
			panic("notify: epoll_wait(2) error - " + err.Error())
		}
	}
}

// TODO(ppknap) : doc.
func (i *inotify) read() (es []*event) {
	n, err := syscall.Read(int(i.fd), i.buffer[:])
	switch {
	case err != nil || n < 0:
		// Panic? error?
		panic(err)
	case n < syscall.SizeofInotifyEvent:
		return
	}
	var sys *syscall.InotifyEvent
	nmin := n - syscall.SizeofInotifyEvent
	for pos, path := 0, ""; pos <= nmin; {
		sys = (*syscall.InotifyEvent)(unsafe.Pointer(&i.buffer[pos]))
		pos += syscall.SizeofInotifyEvent
		if path = ""; sys.Len > 0 {
			endpos := pos + int(sys.Len)
			path = string(bytes.TrimRight(i.buffer[pos:endpos], "\x00"))
			pos = endpos
		}
		es = append(es, &event{
			sys: syscall.InotifyEvent{
				Wd:     sys.Wd,
				Mask:   sys.Mask,
				Cookie: sys.Cookie,
			},
			path: path,
		})
	}
	return
}

// TODO(ppknap) : doc.
func (i *inotify) send(esch <-chan []*event) {
	for es := range esch {
		for _, e := range i.strip(es) {
			if e != nil {
				i.c <- e
			}
		}
	}
	i.wg.Done()
}

// TODO(ppknap) : doc.
func (i *inotify) strip(es []*event) []*event {
	var multi []*event
	i.RLock()
	for idx, e := range es {
		if e.sys.Mask&(syscall.IN_IGNORED|syscall.IN_Q_OVERFLOW) != 0 {
			es[idx] = nil
			continue
		}
		wd, ok := i.m[e.sys.Wd]
		if !ok || e.sys.Mask&encode(Event(wd.mask)) == 0 {
			es[idx] = nil
			continue
		}
		if e.path == "" {
			e.path = wd.path
		} else {
			e.path = filepath.Join(wd.path, e.path)
		}
		multi = append(multi, decode(Event(wd.mask), e))
		if e.event == 0 {
			es[idx] = nil
		}
	}
	i.RUnlock()
	es = append(es, multi...)
	return es
}

// encode converts notify system-independent events to valid inotify mask
// which can be passed to inotify_add_watch(2) function.
func encode(e Event) uint32 {
	if e&Create != 0 {
		e = (e ^ Create) | InCreate | InMovedTo
	}
	if e&Remove != 0 {
		e = (e ^ Remove) | InDelete | InDeleteSelf
	}
	if e&Write != 0 {
		e = (e ^ Write) | InModify
	}
	if e&Rename != 0 {
		e = (e ^ Rename) | InMovedFrom | InMoveSelf
	}
	return uint32(e)
}

// decode uses internally stored mask to distinguish whether system-independent
// or system-dependent event is requested. The first one is created by modifying
// `e` argument. decode method sets e.event value to 0 when an event should be
// skipped. System-dependent event is set as the function's return value which
// can be nil when the event should not be passed on.
func decode(mask Event, e *event) (syse *event) {
	if sysmask := uint32(mask) & e.sys.Mask; sysmask != 0 {
		syse = &event{sys: syscall.InotifyEvent{
			Wd:     e.sys.Wd,
			Mask:   e.sys.Mask,
			Cookie: e.sys.Cookie,
		}, event: Event(sysmask), path: e.path}
	}
	imask := encode(mask)
	switch {
	case mask&Create != 0 && imask&uint32(InCreate|InMovedTo)&e.sys.Mask != 0:
		e.event = Create
	case mask&Remove != 0 && imask&uint32(InDelete|InDeleteSelf)&e.sys.Mask != 0:
		e.event = Remove
	case mask&Write != 0 && imask&uint32(InModify)&e.sys.Mask != 0:
		e.event = Write
	case mask&Rename != 0 && imask&uint32(InMovedFrom|InMoveSelf)&e.sys.Mask != 0:
		e.event = Rename
	default:
		e.event = 0
	}
	return
}

// Unwatch implements notify.watcher interface. It looks for watch descriptor
// related to registered path and if found, calls inotify_rm_watch(2) function.
// This method is allowed to return EINVAL error when concurrently requested to
// delete identical path.
func (i *inotify) Unwatch(path string) (err error) {
	iwd := int32(invalidDescriptor)
	i.RLock()
	for iwdkey, wd := range i.m {
		if wd.path == path {
			iwd = iwdkey
			break
		}
	}
	i.RUnlock()
	if iwd == invalidDescriptor {
		return errNotWatched
	}
	fd := atomic.LoadInt32(&i.fd)
	if _, err = syscall.InotifyRmWatch(int(fd), uint32(iwd)); err != nil {
		return
	}
	i.Lock()
	delete(i.m, iwd)
	i.Unlock()
	return nil
}

// Close implements notify.watcher interface. It removes all existing watch
// descriptors and wakes up producer goroutine by sending data to the write end
// of the pipe. The function waits for a signal from producer which means that
// all operations on current monitoring instance are done.
func (i *inotify) Close() (err error) {
	i.Lock()
	if fd := atomic.LoadInt32(&i.fd); fd == invalidDescriptor {
		i.Unlock()
		return nil
	}
	for iwd := range i.m {
		if _, e := syscall.InotifyRmWatch(int(i.fd), uint32(iwd)); e != nil && err == nil {
			err = e
		}
		delete(i.m, iwd)
	}
	switch _, errwrite := syscall.Write(i.pipefd[1], []byte{0x00}); {
	case errwrite != nil && err == nil:
		err = errwrite
		fallthrough
	case errwrite != nil:
		i.Unlock()
	default:
		i.Unlock()
		i.wg.Wait()
	}
	return
}
