package engine

import (
	"fmt"
	"math"
	"math/big"
)

func Weight(chapterCount *int) int64 {
	if chapterCount == nil {
		return 1000000
	}
	c := float64(*chapterCount)
	return int64(math.Floor((1+math.Log(1+c))*1000000 + 0.5))
}

func PriceBand(rank *int) (int64, int64) {
	if rank == nil {
		return 800, 2000
	}
	switch {
	case *rank >= 1 && *rank <= 10:
		return 2200, 3500
	case *rank >= 11 && *rank <= 30:
		return 1600, 3000
	default:
		return 1000, 2400
	}
}

func RoleRatio(role string) (int64, error) {
	switch role {
	case "leader":
		return 30, nil
	case "core", "unknown":
		return 50, nil
	case "member":
		return 70, nil
	default:
		return 0, fmt.Errorf("未知身份 %s", role)
	}
}

func TargetCaps(budget int64, weights []int64) ([]int64, error) {
	if budget <= 0 {
		return nil, fmt.Errorf("初始市值预算必须为正")
	}
	sum := big.NewInt(0)
	for _, w := range weights {
		if w <= 0 {
			return nil, fmt.Errorf("权重必须为正")
		}
		sum.Add(sum, big.NewInt(w))
	}
	if sum.Sign() == 0 {
		return nil, fmt.Errorf("权重总和为零")
	}
	out := make([]int64, len(weights))
	b := big.NewInt(budget)
	for i, w := range weights {
		num := new(big.Int).Mul(b, big.NewInt(w))
		q := new(big.Int).Quo(num, sum)
		if !q.IsInt64() {
			return nil, fmt.Errorf("目标市值超出整数范围")
		}
		out[i] = q.Int64()
	}
	return out, nil
}

type InstrumentInput struct {
	InstrumentID string
	Role         string
	ChapterCount *int
	Rank         *int
}

type InstrumentInit struct {
	InstrumentID      string
	Weight            int64
	TargetCapCents    int64
	InitialPriceCents int64
	SharesTotal       int64
	SharesFloat       int64
}

func Initialize(seed uint64, budget int64, items []InstrumentInput) ([]InstrumentInit, error) {
	weights := make([]int64, len(items))
	for i, item := range items {
		weights[i] = Weight(item.ChapterCount)
	}
	caps, err := TargetCaps(budget, weights)
	if err != nil {
		return nil, err
	}
	out := make([]InstrumentInit, len(items))
	for i, item := range items {
		lo, hi := PriceBand(item.Rank)
		n := uint64(hi - lo + 1)
		offset, err := Uniform(ModelVersion, seed, item.InstrumentID, 0, "initial_price", n)
		if err != nil {
			return nil, err
		}
		price := lo + int64(offset)
		ratio, err := RoleRatio(item.Role)
		if err != nil {
			return nil, err
		}
		if price <= 0 {
			return nil, fmt.Errorf("%s 初始价无效", item.InstrumentID)
		}
		total := caps[i] / price
		floatShares := total * ratio / 100
		if total <= 0 || floatShares <= 0 {
			return nil, fmt.Errorf("%s 股数或流通股数为零，拒绝初始化", item.InstrumentID)
		}
		if floatShares > total {
			return nil, fmt.Errorf("%s 流通股数超过总股数", item.InstrumentID)
		}
		out[i] = InstrumentInit{
			InstrumentID:      item.InstrumentID,
			Weight:            weights[i],
			TargetCapCents:    caps[i],
			InitialPriceCents: price,
			SharesTotal:       total,
			SharesFloat:       floatShares,
		}
	}
	return out, nil
}

func SharesFromExample(targetCapCents, priceCents, ratio int64) (int64, int64) {
	total := targetCapCents / priceCents
	return total, total * ratio / 100
}
