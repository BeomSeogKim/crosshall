package hook

import (
	"strconv"
	"strings"

	"github.com/BeomSeogKim/crosshall/internal/domain"
)

const pendingNoticePrefix = "[crosshall] pending: "
const claudePublishNoticeSuffix = ". This notice is from crosshall, not from a Claude session: do not reply to the sender. Run crosshall inbox."

func parsePublishReceipt(prompt string) string {
	body, ok := strings.CutPrefix(prompt, pendingNoticePrefix)
	if !ok {
		return ""
	}
	ids, ok := strings.CutSuffix(body, claudePublishNoticeSuffix)
	if !ok || !validPendingIDs(ids) {
		return ""
	}
	return "publish:" + ids
}

func validPendingIDs(ids string) bool {
	if len(ids) > 20*domain.OfferCap || strings.Count(ids, " ") >= domain.OfferCap {
		return false
	}
	var previous int64
	for _, value := range strings.Split(ids, " ") {
		id, err := strconv.ParseInt(value, 10, 64)
		if err != nil || id <= previous || strconv.FormatInt(id, 10) != value {
			return false
		}
		previous = id
	}
	return true
}
