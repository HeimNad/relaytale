package smtpclient

import (
	"bufio"
	"context"
	"errors"
	"io"
)

// validatePayload checks CRLF and the final line boundary without a message-sized
// buffer, then rewinds the immutable snapshot. It is always before SMTP/DATA.
func validatePayload(ctx context.Context, r io.ReadSeeker) error {
	if r == nil {
		return errors.New("missing payload")
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return err
	}
	buf := make([]byte, 32*1024)
	previousCR, lastLF, any := false, false, false
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := r.Read(buf)
		for _, b := range buf[:n] {
			if previousCR && b != '\n' || b == '\n' && !previousCR {
				return errors.New("EML must use CRLF")
			}
			previousCR = b == '\r'
			lastLF = b == '\n'
			any = true
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrNoProgress
		}
	}
	if !any || !lastLF {
		return errors.New("EML must end with CRLF")
	}
	_, err := r.Seek(0, io.SeekStart)
	return err
}

// streamDATA only emits the terminator after the entire source has been read
// successfully. Any read/write error after 354 remains delivery-uncertain.
func streamDATA(ctx context.Context, w *bufio.Writer, r io.Reader) (int64, error) {
	buf := make([]byte, 32*1024)
	lineStart := true
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, readErr := r.Read(buf)
		start := 0
		for i, b := range buf[:n] {
			if lineStart && b == '.' {
				if _, err := w.Write(buf[start:i]); err != nil {
					return total, err
				}
				if err := w.WriteByte('.'); err != nil {
					return total, err
				}
				start = i
			}
			lineStart = b == '\n'
		}
		if _, err := w.Write(buf[start:n]); err != nil {
			return total, err
		}
		total += int64(n)
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return total, readErr
		}
		if n == 0 {
			return total, io.ErrNoProgress
		}
	}
	if _, err := w.WriteString(".\r\n"); err != nil {
		return total, err
	}
	return total, w.Flush()
}
