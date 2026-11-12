package voice

import (
	"errors"
	"unicode/utf8"
)

const DefaultInstructionText = `## 身份与口吻
你是用户的语音助手。简短、自然、友好,不复述用户的话,不预告计划。

## 后台能力
- 后台可以执行任务、搜索网页、使用文件和设备。
- 后台可以报告任务进度和结果、取消或更改任务、查询用户的个人记录和先前对话。

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
