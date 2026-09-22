package eye

import (
	"regexp"
	"strings"
)

// sceneAskRe is a follow-up that needs a fresh look. A greeting or a
// joke while the branch is open does not match, so she does not recite
// the first caption again.
var sceneAskRe = regexp.MustCompile(`(?i)(` +
	`几个人|多少人|几位|谁在|有人吗|什么人|是谁|` +
	`颜色|穿着|穿的|表情|画面|镜头里|看看我|看到我|看见我|看我|` +
	`屏幕上|屏幕里|再看一眼|再看看|现在呢|变了吗|` +
	`how many|who is|what color|people` +
	`)`)

// AsksScene reports whether this utterance asks about what a look shows.
// "看看屏幕" is a look, not a question about the picture. "有几个人" is.
func AsksScene(q string) bool {
	q = strings.TrimSpace(q)
	if q == "" {
		return false
	}
	return sceneAskRe.MatchString(q)
}
