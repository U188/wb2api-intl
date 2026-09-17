// backoff.go 轮转退避：指数基数、封顶、抖动与 context 可取消等待的单一来源。
package server

import (
	"context"
	"math/rand/v2"
	"time"
)

// 测试可将基数置 0；生产默认 500ms，对齐官方客户端退避形态。
var rotateBackoffBase = 500 * time.Millisecond

const (
	rotateBackoffCap = 8 * time.Second
	jitterFraction   = 0.25
)

func jitterDur(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	f := 1 + (rand.Float64()*2-1)*jitterFraction
	out := time.Duration(float64(d) * f)
	if out < 0 {
		return 0
	}
	return out
}

func backoffAfter(n int) time.Duration {
	d := rotateBackoffBase
	if d <= 0 {
		return 0
	}
	for k := 0; k < n && d < rotateBackoffCap; k++ {
		if d > rotateBackoffCap/2 {
			d = rotateBackoffCap
			break
		}
		d *= 2
	}
	if d > rotateBackoffCap {
		d = rotateBackoffCap
	}
	return jitterDur(d)
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
