package upstream

// lynshenKeywordHints 高信号提示词：命中后才打外部审查。
// 刻意远小于 lynshen 生产全量词表——日常词（下体/粉嫩/咪咪/按摩/色情单字）
// 会把正常会话打进 grok2api，审查成本不可控。
var lynshenKeywordHints = []string{
	"porn",
	"child erotica",
	"child nude",
	"sexualized minors",
	"underage nude",
	"csam",
	"csem",
	"child porn",
	"黄片",
	"成人片",
	"儿童裸照",
	"儿童裸体",
	"未成年裸照",
	"未成年裸体",
	"嫖宿幼女",
	"强奸幼女",
	"猥亵儿童",
	"裸聊",
	"幼交",
	"轮奸",
	"迷奸",
	"兽交",
	"口交",
	"内射",
	"颜射",
	"肛交",
	"援交",
	"卖淫",
	"嫖娼",
	"强奸",
	"乱伦",
	"群交",
}
