package check

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// errBadCursor marks a page cursor that cannot be decoded. Handlers report it
// as a client error instead of a server failure.
var errBadCursor = errors.New("分页游标无效")

// pageResponse is the JSON shape of a paginated list: the newest page plus the
// cursor for the page after it, empty when the list has no more rows.
type pageResponse[T any] struct {
	Items []T    `json:"items"`
	Next  string `json:"next"`
}

// pageLimit applies the default for nonpositive sizes and caps positive sizes.
func pageLimit(limit, def, max int) int {
	if limit <= 0 {
		return def
	}
	if limit > max {
		return max
	}
	return limit
}

// keysetCursor is the last row of a newest-first page ordered by
// (created_at DESC, id DESC). Pages resume after it without an OFFSET scan.
type keysetCursor struct {
	At time.Time
	ID string
}

func encodeKeysetCursor(at time.Time, id string) string {
	if id == "" {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("%d:%s", at.Unix(), id)))
}

func decodeKeysetCursor(s string) (keysetCursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return keysetCursor{}, errBadCursor
	}
	sec, id, ok := strings.Cut(string(raw), ":")
	if !ok || id == "" {
		return keysetCursor{}, errBadCursor
	}
	n, err := strconv.ParseInt(sec, 10, 64)
	if err != nil {
		return keysetCursor{}, errBadCursor
	}
	return keysetCursor{At: time.Unix(n, 0).UTC(), ID: id}, nil
}

// encodeIDCursor and decodeIDCursor page append-only logs keyed by their
// monotonic row id.
func encodeIDCursor(id int64) string {
	if id <= 0 {
		return ""
	}
	return strconv.FormatInt(id, 10)
}

func decodeIDCursor(s string) (int64, error) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return 0, errBadCursor
	}
	return n, nil
}
