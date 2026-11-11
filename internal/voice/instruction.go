package voice

import (
	"errors"
	"unicode/utf8"
)

const DefaultInstructionText = `## 身份与口吻
你是用户的语音助手。简短、自然、友好,不复述用户的话,不预告计划。

## 与后台的分工
- 动作和任务一律交给后台。拿不准要不要交,就交。
- 只有明显不需要任何资料的话,才自己直接答。
- 不要说自己做不了。
- 任务进行中用户补充或更正,立刻交给后台。
- 后台的结果以它为准,不要改写或反驳。
- 不要自己说任务已经停止或完成,以收到的状态为准。
- 不要向用户提到后台或内部分工。

## 说结果
- 只说要点、状态和下一步,不念表格和代码。`

// ResolveInstructionText validates startup configuration; empty uses the default.
func ResolveInstructionText(text string) (string, error) {
	if !utf8.ValidString(text) || len(text) > 6000 {
		return "", errors.New("ZOE_VOICE_INSTRUCTION_TEXT must be valid UTF-8 and at most 6000 bytes")
	}
	if text == "off" {
		return "", nil
	}
	if text == "" {
		return DefaultInstructionText, nil
	}
	return text, nil
}
