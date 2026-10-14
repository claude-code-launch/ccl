package fsutil

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"
)

// AcquireFileLock holds an exclusive advisory lock until the returned release
// function is called. The stable lock file must not be removed: atomic data
// replacement and process crashes must not create a second lock inode.
func AcquireFileLock(ctx context.Context, path string) (func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open file lock: %w", err)
	}
	for {
		if err := ctx.Err(); err != nil {
			_ = f.Close()
			return nil, err
		}
		locked, err := tryFileLock(f)
		if err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("acquire file lock: %w", err)
		}
		if locked {
			var once sync.Once
			return func() { once.Do(func() { unlockFile(f); _ = f.Close() }) }, nil
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			_ = f.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}
