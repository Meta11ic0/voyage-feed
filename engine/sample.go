package engine

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"strconv"
)

const ModelVersion = "random-v1"

func SampleUint(model string, seed uint64, instrumentID string, tick int64, purpose string, retry uint64) uint64 {
	payload := model + "\n" +
		strconv.FormatUint(seed, 10) + "\n" +
		instrumentID + "\n" +
		strconv.FormatInt(tick, 10) + "\n" +
		purpose + "\n" +
		strconv.FormatUint(retry, 10)
	sum := sha256.Sum256([]byte(payload))
	return binary.BigEndian.Uint64(sum[:8])
}

func SampleDigest(model string, seed uint64, instrumentID string, tick int64, purpose string, retry uint64) string {
	payload := model + "\n" +
		strconv.FormatUint(seed, 10) + "\n" +
		instrumentID + "\n" +
		strconv.FormatInt(tick, 10) + "\n" +
		purpose + "\n" +
		strconv.FormatUint(retry, 10)
	sum := sha256.Sum256([]byte(payload))
	return fmt.Sprintf("%x", sum[:])
}

func Uniform(model string, seed uint64, instrumentID string, tick int64, purpose string, n uint64) (uint64, error) {
	if n == 0 {
		return 0, fmt.Errorf("抽样上界必须为正")
	}
	threshold := (uint64(0) - n) % n
	for retry := uint64(0); retry < 100000; retry++ {
		r := SampleUint(model, seed, instrumentID, tick, purpose, retry)
		if r < threshold {
			continue
		}
		return r % n, nil
	}
	return 0, fmt.Errorf("抽样重试次数过多")
}
