// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package scanner is the replacer of the stdlib `bufio.Scanner`,
// which adds the read offset.
package scanner

import (
	"bufio"
	"io"
)

const (
	maxConsecutiveEmptyReads = 100
	startBufSize             = 4096 // Size of initial allocation for buffer.
)

var scannerHandleRead = func(*Scanner, int) bool { return false }

// Scanner provides a convenient interface for reading data such as
// a file of newline-delimited lines of text. Successive calls to
// the Scan method will step through the 'tokens' of a file, skipping
// the bytes between the tokens. The specification of a token is
// defined by a split function of type SplitFunc; the default split
// function breaks the input into lines with line termination stripped. Split
// functions are defined in this package for scanning a file into
// lines, bytes, UTF-8-encoded runes, and space-delimited words. The
// client may instead provide a custom split function.
//
// Scanning stops unrecoverably at EOF, the first I/O error, or a token too
// large to fit in the buffer. When a scan stops, the reader may have
// advanced arbitrarily far past the last token. Programs that need more
// control over error handling or large tokens, or must run sequential scans
// on a reader, should use bufio.Reader instead.
//
// Notice: it is a replacer of the stdlib bufio.Scanner with the read offset.
type Scanner struct {
	offset       int             // The offset of the reader from the start.
	r            io.Reader       // The reader provided by the client.
	split        bufio.SplitFunc // The function to split the tokens.
	maxTokenSize int             // Maximum size of a token; modified by tests.
	token        []byte          // Last token returned by split.
	buf          []byte          // Buffer used as argument to split.
	start        int             // First non-processed byte in buf.
	end          int             // End of data in buf.
	err          error           // Sticky error.
	empties      int             // Count of successive empty tokens.
	scanCalled   bool            // Scan has been called; buffer is in use.
	done         bool            // Scan has finished.
}

// NewScanner returns a new Scanner to read from r.
// The split function defaults to bufio.ScanLines.
func NewScanner(r io.Reader) *Scanner {
	return NewScannerWithOffset(r, 0)
}

// NewScannerWithOffset returns a new Scanner to read from r with offset.
// The split function defaults to bufio.ScanLines.
func NewScannerWithOffset(r io.Reader, offset int) *Scanner {
	return &Scanner{
		r:            r,
		offset:       offset,
		split:        bufio.ScanLines,
		maxTokenSize: bufio.MaxScanTokenSize,
	}
}

// Err returns the first non-EOF error that was encountered by the Scanner.
func (s *Scanner) Err() error {
	if s.err == io.EOF {
		return nil
	}
	return s.err
}

// Bytes returns the most recent token generated by a call to Scan.
// The underlying array may point to data that will be overwritten
// by a subsequent call to Scan. It does no allocation.
func (s *Scanner) Bytes() []byte {
	return s.token
}

// Text returns the most recent token generated by a call to Scan
// as a newly allocated string holding its bytes.
func (s *Scanner) Text() string {
	return string(s.token)
}

// Offset returns the offset of the used data read from the reader.
func (s *Scanner) Offset() int {
	return s.offset
}

// Scan advances the Scanner to the next token, which will then be
// available through the Bytes or Text method. It returns false when the
// scan stops, either by reaching the end of the input or an error.
// After Scan returns false, the Err method will return any error that
// occurred during scanning, except that if it was io.EOF, Err
// will return nil.
// Scan panics if the split function returns too many empty
// tokens without advancing the input. This is a common error mode for
// scanners.
func (s *Scanner) Scan() bool {
	if s.done {
		return false
	}
	s.scanCalled = true
	// Loop until we have a token.
	for {
		// See if we can get a token with what we already have.
		// If we've run out of data but have an error, give the split function
		// a chance to recover any remaining, possibly empty token.
		if s.end > s.start || s.err != nil {
			advance, token, err := s.split(s.buf[s.start:s.end], s.err != nil)
			s.offset += advance
			if err != nil {
				if err == bufio.ErrFinalToken {
					s.token = token
					s.done = true
					return true
				}
				s.setErr(err)
				return false
			}
			if !s.advance(advance) {
				return false
			}
			s.token = token
			if token != nil {
				if s.err == nil || advance > 0 {
					s.empties = 0
				} else {
					// Returning tokens not advancing input at EOF.
					s.empties++
					if s.empties > maxConsecutiveEmptyReads {
						panic("bufio.Scan: too many empty tokens without progressing")
					}
				}
				return true
			}
		}
		// We cannot generate a token with what we are holding.
		// If we've already hit EOF or an I/O error, we are done.
		if s.err != nil {
			// Shut it down.
			s.start = 0
			s.end = 0
			return false
		}
		// Must read more data.
		// First, shift data to beginning of buffer if there's lots of empty space
		// or space is needed.
		if s.start > 0 && (s.end == len(s.buf) || s.start > len(s.buf)/2) {
			copy(s.buf, s.buf[s.start:s.end])
			s.end -= s.start
			s.start = 0
		}
		// Is the buffer full? If so, resize.
		if s.end == len(s.buf) {
			// Guarantee no overflow in the multiplication below.
			const maxInt = int(^uint(0) >> 1)
			if len(s.buf) >= s.maxTokenSize || len(s.buf) > maxInt/2 {
				s.setErr(bufio.ErrTooLong)
				return false
			}
			newSize := len(s.buf) * 2
			if newSize == 0 {
				newSize = startBufSize
			}
			if newSize > s.maxTokenSize {
				newSize = s.maxTokenSize
			}
			newBuf := make([]byte, newSize)
			copy(newBuf, s.buf[s.start:s.end])
			s.buf = newBuf
			s.end -= s.start
			s.start = 0
		}
		// Finally we can read some input. Make sure we don't get stuck with
		// a misbehaving Reader. Officially we don't need to do this, but let's
		// be extra careful: Scanner is for safe, simple jobs.
		for loop := 0; ; {
			n, err := s.r.Read(s.buf[s.end:len(s.buf)])
			if scannerHandleRead(s, n) {
				break
			}
			s.end += n
			if err != nil {
				s.setErr(err)
				break
			}
			if n > 0 {
				s.empties = 0
				break
			}
			loop++
			if loop > maxConsecutiveEmptyReads {
				s.setErr(io.ErrNoProgress)
				break
			}
		}
	}
}

// advance consumes n bytes of the buffer. It reports whether the advance was legal.
func (s *Scanner) advance(n int) bool {
	if n < 0 {
		s.setErr(bufio.ErrNegativeAdvance)
		return false
	}
	if n > s.end-s.start {
		s.setErr(bufio.ErrAdvanceTooFar)
		return false
	}
	s.start += n
	return true
}

// setErr records the first error encountered.
func (s *Scanner) setErr(err error) {
	if s.err == nil || s.err == io.EOF {
		s.err = err
	}
}

// Buffer sets the initial buffer to use when scanning and the maximum
// size of buffer that may be allocated during scanning. The maximum
// token size is the larger of max and cap(buf). If max <= cap(buf),
// Scan will use this buffer only and do no allocation.
//
// By default, Scan uses an internal buffer and sets the
// maximum token size to MaxScanTokenSize.
//
// Buffer panics if it is called after scanning has started.
func (s *Scanner) Buffer(buf []byte, max int) {
	if s.scanCalled {
		panic("Buffer called after Scan")
	}
	s.buf = buf[0:cap(buf)]
	s.maxTokenSize = max
}

// Split sets the split function for the Scanner.
// The default split function is ScanLines.
//
// Split panics if it is called after scanning has started.
func (s *Scanner) Split(split bufio.SplitFunc) {
	if s.scanCalled {
		panic("Split called after Scan")
	}
	s.split = split
}
