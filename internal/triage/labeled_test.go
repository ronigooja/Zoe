package triage

import (
	"context"
	"fmt"
	"os"
	"testing"
)

type labeledTriageSentence struct {
	Text     string
	Category Category
}

var labeledTriageSentences = []labeledTriageSentence{
	{"好了吗", Progress}, {"弄好了没有", Progress}, {"还要多久啊", Progress}, {"查到了吗", Progress}, {"现在到哪步了呀", Progress},
	{"我上周三跟你说了什么", Recall}, {"昨天我们聊了啥来着", Recall}, {"上次那个方案是啥", Recall}, {"前几天我让你做了什么", Recall}, {"我之前说过的地址呢", Recall},
	{"记住我喜欢喝美式", Memory}, {"我的车牌是多少", Memory}, {"我对花生过敏你记一下", Memory}, {"把我女儿生日记住啊", Memory}, {"我常用的邮箱你还记得不", Memory},
	{"帮我查一下今天的天气", Task}, {"把这份文档总结一下", Task}, {"订明天的闹钟", Task}, {"整理下载文件夹", Task}, {"呃帮我搜搜附近的药店", Task},
	{"你好", Chat}, {"谢谢你啊", Chat}, {"你今天心情怎么样", Chat}, {"讲个笑话呗", Chat}, {"我觉得春天最好", Chat},
}

func TestLabeledSetCoversEveryCategoryAtLeastFiveTimes(t *testing.T) {
	counts := map[Category]int{}
	for _, sentence := range labeledTriageSentences {
		counts[sentence.Category]++
	}
	for _, category := range []Category{Progress, Recall, Memory, Task, Chat} {
		if counts[category] < 5 {
			t.Errorf("%s has %d labeled sentences", category, counts[category])
		}
	}
}

func TestLiveTriageLabeledSet(t *testing.T) {
	if os.Getenv("ZOE_TRIAGE_LIVE_EVAL") != "1" {
		t.Skip("set ZOE_TRIAGE_LIVE_EVAL=1 to run against the configured endpoint")
	}
	client, err := FromEnv(os.Getenv, os.ReadFile)
	if err != nil || client == nil {
		t.Fatal(err)
	}
	correct := map[Category]int{}
	total := map[Category]int{}
	for _, sentence := range labeledTriageSentences {
		got, classifyErr := client.Classify(context.Background(), sentence.Text)
		total[sentence.Category]++
		if classifyErr == nil && got == sentence.Category {
			correct[sentence.Category]++
		} else {
			fmt.Printf("misclassified expected=%s got=%s text=%s err=%v\n", sentence.Category, got, sentence.Text, classifyErr)
		}
	}
	for _, category := range []Category{Progress, Recall, Memory, Task, Chat} {
		fmt.Printf("%s: %d/%d (%.1f%%)\n", category, correct[category], total[category], 100*float64(correct[category])/float64(total[category]))
	}
}
