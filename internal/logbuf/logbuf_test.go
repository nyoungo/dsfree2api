package logbuf

import (
	"testing"
	"time"
)

// 回归：Add 必须在持锁状态下发送，否则会与 Subscribe 的取消闭包（锁内
// close(c)，logbuf.go:82-89）竞争 —— 向已关闭的 channel 发送是 panic，
// 发生在后台 goroutine 时整个进程都会挂。
//
// 订阅者越多，遍历发送的时间越长，越容易命中这个窗口。
func TestAddConcurrentUnsubscribeDoesNotFatal(t *testing.T) {
	b := New(1000)

	const n = 8000
	unsubs := make([]func(), 0, n)
	for i := 0; i < n; i++ {
		_, unsub := b.Subscribe()
		unsubs = append(unsubs, unsub)
	}

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				b.Add(Entry{Level: "INFO", Msg: "probe"})
			}
		}
	}()

	time.Sleep(20 * time.Millisecond)
	for _, u := range unsubs {
		u()
	}
	close(stop)
	<-done
}
