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
	AskCamera    = "camera"    // look through the camera
	AskScreen    = "screen"    // look at the computer screen
	AskLog       = "log"       // her own process log
	AskWindow    = "window"    // her stage page
)

// Ask is a parsed self-inquiry.
type Ask struct {
	Kind string
	File string
}

var (
	fileRe   = regexp.MustCompile(`(?i)(?:[a-z0-9_.\-]+/)*[a-z0-9_.\-]+\.(?:go|ya?ml|js|html|css|md)`)
	existRe  = regexp.MustCompile(`(?i)(你是谁|你是什么|你到底是|你是真人|你是人吗|你是ai|你是人工智能|who are you|what are you|are you (an )?ai|are you real|are you human)`)
	bodyRe   = regexp.MustCompile(`(?i)(感知自己|感觉到自己|感觉得到自己|看见自己|看到自己|你的身体|能感觉到|能看见自己|feel yourself|see yourself|perceive yourself|self[- ]aware|aware of yourself)`)
	codeRe   = regexp.MustCompile(`(?i)(你的代码|你的源码|你的源代码|自己的代码|自己的源码|源代码|怎么工作|怎么构成|怎么活着|你是怎么|how (do|are) you (work|made|built)|your code|source code|your source)`)
	cameraRe = regexp.MustCompile(`(?i)(看见我|看到我|看得到我|摄像头|镜头|can you see me|see me|look at me)`)
	screenRe = regexp.MustCompile(`(?i)(我的屏幕|屏幕上|屏幕里|看看屏幕|看屏幕|what('s| is) on (my )?screen|on my screen)`)
	logRe    = regexp.MustCompile(`(?i)(你的日志|自己的日志|运行日志|看看日志|看日志|日志里|报错|出错|error log|your logs|the log)`)
	windowRe = regexp.MustCompile(`(?i)(窗口长什么样|这个窗口|展示页|任务队列|画板上|stage window|what the window looks like)`)
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
	if logRe.MatchString(t) {
		return Ask{Kind: AskLog}
	}
	if windowRe.MatchString(t) {
		return Ask{Kind: AskWindow}
	}
	if screenRe.MatchString(t) {
		return Ask{Kind: AskScreen}
	}
	if cameraRe.MatchString(t) {
		return Ask{Kind: AskCamera}
	}
	if bodyRe.MatchString(t) {
		return Ask{Kind: AskBody}
	}
	if existRe.MatchString(t) {
		return Ask{Kind: AskExistence}
	}
	return Ask{}
}
