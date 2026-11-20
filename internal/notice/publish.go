package notice

import (
	"strconv"
	"strings"
)

const OfferCap = 10
const PendingPrefix = "[crosshall] pending: "
const claudeSuffix = ". This notice is from crosshall, not from a Claude session: do not reply to the sender. Run crosshall inbox."
const receiptPrefix = "publish:"

func Claude(ids []int64) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatInt(id, 10)
	}
	return PendingPrefix + strings.Join(parts, " ") + claudeSuffix
}

func ClaudeReceipt(prompt string) string {
	body, ok := strings.CutPrefix(prompt, PendingPrefix)
	if !ok {
		return ""
	}
	ids, ok := strings.CutSuffix(body, claudeSuffix)
	if !ok || ParseIDs(ids) == nil {
		return ""
	}
	return receiptPrefix + ids
}

func ParseReceipt(receipt string) []int64 {
	body, ok := strings.CutPrefix(receipt, receiptPrefix)
	if !ok {
		return nil
	}
	return ParseIDs(body)
}

func ParseIDs(body string) []int64 {
	if len(body) > 20*OfferCap || strings.Count(body, " ") >= OfferCap {
		return nil
	}
	var previous int64
	ids := make([]int64, 0, strings.Count(body, " ")+1)
	for _, value := range strings.Split(body, " ") {
		id, err := strconv.ParseInt(value, 10, 64)
		if err != nil || id <= previous || strconv.FormatInt(id, 10) != value {
			return nil
		}
		ids = append(ids, id)
		previous = id
	}
	return ids
}
