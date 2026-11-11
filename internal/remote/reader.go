package remote

import "io"

// exactReader yields exactly the number of bytes promised by the Content-Range of a block
// response. A body that ends sooner is an error and not a short block, because the caller
// would otherwise cache a truncated block as if it were complete.
type exactReader struct {
	body      io.ReadCloser
	remaining int64
}

func (r *exactReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.body.Read(p)
	r.remaining -= int64(n)
	if err == io.EOF && r.remaining > 0 {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}

func (r *exactReader) Close() error {
	return r.body.Close()
}
