package sense

import (
	"path"
	"regexp"
	"strings"
)

// Ask is what the user seems to be asking about her.
const (
	AskNone      = ""
	AskExistence = "existence" // who/what are you
	AskBody      = "body"      // can you feel/see yourself
	AskCode      = "code"      // how are you made / your source
	AskFile      = "file"      // a specific file in her body
	AskSee       = "see"       // camera / screen
)

// Ask is a parsed self-inquiry.
type Ask struct {
	Kind string
	File string
}

var (
	fileRe  = regexp.MustCompile(`(?i)(?:[a-z0-9_.\-]+/)*[a-z0-9_.\-]+\.(?:go|ya?ml|js|html|css|md)`)
	existRe = regexp.MustCompile(`(?i)(你是谁|你是什么|你到底是|你是真人|你是人吗|你是ai|你是人工智能|who are you|what are you|are you (an )?ai|are you real|are you human)`)
	bodyRe  = regexp.MustCompile(`(?i)(感知自己|感觉到自己|感觉得到自己|看见自己|看到自己|你的身体|能感觉到|能看见自己|feel yourself|see yourself|perceive yourself|self[- ]aware|aware of yourself)`)
	codeRe  = regexp.MustCompile(`(?i)(你的代码|你的源码|你的源代码|自己的代码|自己的源码|源代码|怎么工作|怎么构成|怎么活着|你是怎么|how (do|are) you (work|made|built)|your code|source code|your source)`)
	seeRe   = regexp.MustCompile(`(?i)(看见我|看到我|看得到我|看得到吗|你能看见|你看得见|摄像头|镜头|我的屏幕|屏幕上|屏幕里|你在看什么|看见什么|can you see( me)?|see me|look at me|what('s| is) on (my )?screen|do you see)`)
)

// ParseAsk classifies a user utterance. File > code > body > existence.
func ParseAsk(text string) Ask {
	t := strings.TrimSpace(text)
	if t == "" {
		return Ask{}
	}
	if m := fileRe.FindString(t); m != "" {
		rel := path.Clean(strings.ReplaceAll(m, "\\", "/"))
		if allowed(rel) {
			return Ask{Kind: AskFile, File: rel}
		}
		return Ask{Kind: AskCode, File: rel}
	}
	if codeRe.MatchString(t) {
		return Ask{Kind: AskCode}
	}
	if seeRe.MatchString(t) {
		return Ask{Kind: AskSee}
	}
	if bodyRe.MatchString(t) {
		return Ask{Kind: AskBody}
	}
	if existRe.MatchString(t) {
		return Ask{Kind: AskExistence}
	}
	return Ask{}
}
