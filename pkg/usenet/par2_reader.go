package usenet

import (
	"context"
	"errors"
	"io"

	"github.com/Trifocals3537/tessarr/pkg/storage"
	"github.com/Trifocals3537/tessarr/pkg/usenet/fs/reader"
	"github.com/Trifocals3537/tessarr/pkg/usenet/recovery"
)

func (u *Usenet) recoveredArticle(file *storage.NZBFile) func(context.Context, reader.SegmentMeta, io.Writer) (bool, error) {
	if u.recovery == nil || recovery.Eligible(file) != nil {
		return nil
	}
	gen, size, store := recovery.Generation(*file), file.Size, u.recovery.Store()
	return func(ctx context.Context, seg reader.SegmentMeta, w io.Writer) (bool, error) {
		if seg.SegmentDataStart != 0 || !store.CoversContext(ctx, gen, seg.StartOffset, seg.Bytes) {
			return false, nil
		}
		buffer := make([]byte, min(seg.Bytes, int64(64<<10)))
		for off, remaining := seg.StartOffset, seg.Bytes; remaining > 0; {
			want := min(remaining, int64(len(buffer)))
			n, err := store.Overlay(ctx, gen, size, buffer[:want], off, func(context.Context, []byte, int64) (int, error) { return 0, errors.New("recovery coverage changed") })
			if err != nil {
				return false, err
			}
			if int64(n) != want {
				return false, io.ErrUnexpectedEOF
			}
			written, err := w.Write(buffer[:n])
			if err != nil {
				return false, err
			}
			if written != n {
				return false, io.ErrShortWrite
			}
			off += want
			remaining -= want
		}
		return true, nil
	}
}
