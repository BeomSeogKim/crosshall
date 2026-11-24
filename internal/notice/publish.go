package notice

import (
	"strconv"
	"strings"
)

const OfferCap = 10
const PendingPrefix = "[crosshall] pending: "
const receiptPrefix = "publish:"

// resumeTail ends a resume notice, after the ID list it repeats.
const resumeTail = " to see them again, carry on, and use crosshall reply to answer each, ask for more time or decline, as the member rules say."

// ResumeHead is what a resume notice says between its ID list and the
// same list repeated, for an agent's sessions ("Claude", "Codex").
func ResumeHead(agent string) string {
	return ". This notice is from crosshall, not from a " + agent +
		" session. The hall restarted and you still owe these requests: run crosshall read "
}

// Resume renders the notice that names requests a member still owes after
// the hall restarted: they were read before, so it names them to read again.
func Resume(head string, ids []int64) string {
	list := IDList(ids)
	return PendingPrefix + list + head + list + resumeTail
}

// CutResume returns the ID list of a resume notice body, the prompt after
// PendingPrefix: the list, head, the same list, and the tail.
func CutResume(head, body string) (string, bool) {
	i := strings.Index(body, head)
	if i <= 0 {
		return "", false
	}
	ids := body[:i]
	rest := body[i+len(head):]
	if len(rest) != len(ids)+len(resumeTail) || !strings.HasPrefix(rest, ids) || !strings.HasSuffix(rest, resumeTail) {
		return "", false
	}
	return ids, true
}

// IDList writes message IDs as a notice carries them.
func IDList(ids []int64) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatInt(id, 10)
	}
	return strings.Join(parts, " ")
}

// Receipt is the receipt for an ID list an adapter recognized in a
// submitted prompt, or "" when the list is not valid.
func Receipt(ids string) string {
	if ParseIDs(ids) == nil {
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
