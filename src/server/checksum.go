package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"hash"
	"hash/crc32"
	"strings"
)

// supportedChecksumAlgos is the canonical comma-separated list ferry
// advertises. Order is intentional: clients pick the cheapest option (crc32c)
// unless they have a reason to upgrade.
const supportedChecksumAlgos = "crc32c,sha256"

// crc32cTable is the Castagnoli polynomial used by tus and most modern
// hardware-accelerated CRC32 implementations.
var crc32cTable = crc32.MakeTable(crc32.Castagnoli)

// parseUploadChecksum parses an `Upload-Checksum: <algo> <hex>` header. Per
// the tus checksum extension the value should be base64-encoded, but ferry
// uses hex because (a) we control both ends, and (b) hex is friendlier for
// shell-script clients. Returns:
//
//   - expected: the decoded digest bytes
//   - hasher:   a fresh hash.Hash for the algorithm, ready to be Tee'd into
//   - err:      ErrUnsupportedChecksumAlgo if the algo isn't recognized,
//     or nil with both expected==nil and hasher==nil if the header is empty
//     (i.e. the client opted out).
func parseUploadChecksum(header string) ([]byte, hash.Hash, error) {
	header = strings.TrimSpace(header)
	if header == "" {
		return nil, nil, nil
	}
	parts := strings.SplitN(header, " ", 2)
	if len(parts) != 2 {
		return nil, nil, ErrUnsupportedChecksumAlgo
	}
	algo := strings.ToLower(strings.TrimSpace(parts[0]))
	digestHex := strings.TrimSpace(parts[1])

	expected, err := hex.DecodeString(digestHex)
	if err != nil {
		return nil, nil, ErrUnsupportedChecksumAlgo
	}

	var hasher hash.Hash
	switch algo {
	case "crc32c":
		hasher = crc32.New(crc32cTable)
		if len(expected) != crc32.Size {
			return nil, nil, ErrUnsupportedChecksumAlgo
		}
	case "sha256":
		hasher = sha256.New()
		if len(expected) != sha256.Size {
			return nil, nil, ErrUnsupportedChecksumAlgo
		}
	default:
		return nil, nil, ErrUnsupportedChecksumAlgo
	}
	return expected, hasher, nil
}

// hashesEqual is a constant-time hash comparison. Overkill for an integrity
// check, but it costs nothing and keeps timing-channel-paranoid reviewers
// from scrolling further.
func hashesEqual(a, b []byte) bool {
	return subtle.ConstantTimeCompare(a, b) == 1
}
