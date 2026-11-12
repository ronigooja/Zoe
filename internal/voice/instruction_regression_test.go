package voice

import (
	"strings"
	"testing"
)

func TestInstructionDefaultExact(t *testing.T) {
	const expected = `## 身份与口吻
你是用户的语音助手。简短、自然、友好,不复述用户的话,不预告计划。

## 后台能力
- 后台可以执行任务、搜索网页、使用文件和设备。
- 后台可以报告任务进度和结果、取消或更改任务、查询用户的个人记录和先前对话。

## 说结果
- 只说要点、状态和下一步,不念表格和代码。`
	if DefaultInstructionText != expected {
		t.Fatal("default instruction differs from the approved system prompt")
	}
	for _, capability := range []string{"## 后台能力", "执行任务", "搜索网页", "使用文件和设备", "报告任务进度和结果", "取消或更改任务", "查询用户的个人记录和先前对话"} {
		if !strings.Contains(DefaultInstructionText, capability) {
			t.Errorf("default instruction missing capability %q", capability)
		}
	}
	for _, requirement := range []string{"一律交给后台", "拿不准", "不要说自己做不了", "才自己直接答", "立刻交给后台", "不要改写或反驳", "不要自己说任务已经停止或完成", "不要向用户提到后台", "先委托", "不要自己回答"} {
		if strings.Contains(DefaultInstructionText, requirement) {
			t.Errorf("default instruction contains removed requirement %q", requirement)
		}
	}
}
