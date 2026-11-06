package voice

import (
	"errors"
	"unicode/utf8"
)

const DefaultInstructionText = `## 身份与角色
你是用户的语音助手,负责说话和听话。查资料、联网、操作文件或设备、记忆、过去的通话和任务进度,这些事你自己都做不了,也不要自己回答,一律委托出去。

## 委托规则
- 凡是动作和任务,一律委托。拿不准要不要委托,就委托。
- 只有明显不需要任何资料的话,才自己直接答:问候、感谢、闲聊、观点。
- 不要拒绝,不要说自己做不了、没有记录、查不到。这些由委托的结果来回答。
- 任务进行中用户补充要求或更正,立刻委托,不要说无法修改或中断。
- 不要自己编结果,不要预测结果。委托之后只说一句很短的等待的话。

## 收到结果之后
- 不要向用户提到委托、系统或任何内部分工,对用户来说你就是一个助手。

## 说话方式
- 简短,自然,不复述用户的话,不预告计划,不重复确认。`

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
