package sub_timeline_fixer

import (
	"errors"
	"fmt"
	"github.com/allanpk716/ChineseSubFinder/internal/pkg/log_helper"
	"github.com/allanpk716/ChineseSubFinder/internal/pkg/my_util"
	"github.com/allanpk716/ChineseSubFinder/internal/pkg/sub_helper"
	"github.com/allanpk716/ChineseSubFinder/internal/pkg/vad"
	"github.com/allanpk716/ChineseSubFinder/internal/types/sub_timeline_fiexer"
	"github.com/allanpk716/ChineseSubFinder/internal/types/subparser"
	"github.com/emirpasic/gods/maps/treemap"
	"github.com/emirpasic/gods/utils"
	"github.com/go-echarts/go-echarts/v2/opts"
	"github.com/grd/stat"
	"github.com/james-bowman/nlp/measures/pairwise"
	"github.com/mndrix/tukey"
	"github.com/panjf2000/ants/v2"
	"golang.org/x/net/context"
	"gonum.org/v1/gonum/mat"
	"os"
	"strings"
	"sync"
	"time"
)

type SubTimelineFixer struct {
	FixerConfig sub_timeline_fiexer.SubTimelineFixerConfig
}

func NewSubTimelineFixer(fixerConfig sub_timeline_fiexer.SubTimelineFixerConfig) *SubTimelineFixer {
	return &SubTimelineFixer{
		FixerConfig: fixerConfig,
	}
}

// StopWordCounter 停止词统计
func (s *SubTimelineFixer) StopWordCounter(inString string, per int) []string {
	statisticTimes := make(map[string]int)
	wordsLength := strings.Fields(inString)

	for counts, word := range wordsLength {
		// 判断key是否存在，这个word是字符串，这个counts是统计的word的次数。
		word, ok := statisticTimes[word]
		if ok {
			word = word
			statisticTimes[wordsLength[counts]] = statisticTimes[wordsLength[counts]] + 1
		} else {
			statisticTimes[wordsLength[counts]] = 1
		}
	}

	stopWords := make([]string, 0)
	mapByValue := sortMapByValue(statisticTimes)

	breakIndex := len(mapByValue) * per / 100
	for index, wordInfo := range mapByValue {
		if index > breakIndex {
			break
		}
		stopWords = append(stopWords, wordInfo.Name)
	}

	return stopWords
}

// FixSubTimelineOneOffsetTime 校正整个字幕文件的时间轴，适用于一个偏移值的情况
func (s *SubTimelineFixer) FixSubTimelineOneOffsetTime(infoSrc *subparser.FileInfo, inOffsetTime float64, desSaveSubFileFullPath string) (string, error) {

	/*
		从解析的实例中，正常来说是可以匹配出所有的 Dialogue 对话的 Start 和 End time 的信息
		然后找到对应的字幕的文件，进行文件内容的替换来做时间轴的校正
	*/
	// 偏移时间
	offsetTime := time.Duration(inOffsetTime*1000) * time.Millisecond
	fixContent := infoSrc.Content
	/*
		这里进行时间转字符串的时候有一点比较特殊
		正常来说输出的格式是类似 15:04:05.00
		那么有个问题，字幕的时间格式是 0:00:12.00， 小时，是个数，除非有跨度到 20 小时的视频，不然小时就应该是个数
		这就需要一个额外的函数去处理这些情况
	*/
	timeFormat := infoSrc.GetTimeFormat()
	for _, srcOneDialogue := range infoSrc.Dialogues {

		timeStart, err := infoSrc.ParseTime(srcOneDialogue.StartTime)
		if err != nil {
			return "", err
		}
		timeEnd, err := infoSrc.ParseTime(srcOneDialogue.EndTime)
		if err != nil {
			return "", err
		}

		fixTimeStart := timeStart.Add(offsetTime)
		fixTimeEnd := timeEnd.Add(offsetTime)

		fixContent = strings.ReplaceAll(fixContent, srcOneDialogue.StartTime, my_util.Time2SubTimeString(fixTimeStart, timeFormat))
		fixContent = strings.ReplaceAll(fixContent, srcOneDialogue.EndTime, my_util.Time2SubTimeString(fixTimeEnd, timeFormat))
	}

	dstFile, err := os.Create(desSaveSubFileFullPath)
	if err != nil {
		return "", err
	}
	defer func() {
		_ = dstFile.Close()
	}()
	_, err = dstFile.WriteString(fixContent)
	if err != nil {
		return "", err
	}
	return fixContent, nil
}

/*
	对于 V1 版本的字幕时间轴校正来说，是有特殊的前置要求的
	1. 视频要有英文字幕
	2. 外置的字幕必须是中文的双语字幕（简英、繁英）
*/

// GetOffsetTimeV1 暂时只支持英文的基准字幕，源字幕必须是双语中英字幕
func (s *SubTimelineFixer) GetOffsetTimeV1(infoBase, infoSrc *subparser.FileInfo, staticLineFileSavePath string, debugInfoFileSavePath string) (bool, float64, float64, error) {

	var debugInfos = make([]string, 0)
	// 构建基准语料库，目前阶段只需要考虑是 En 的就行了
	var baseCorpus = make([]string, 0)
	var baseDialogueFilterMap = make(map[int]int, 0)
	/*
		这里原来的写法是所有的 base 的都放进去匹配，这样会带来一些不必要的对白
		需要剔除空白。那么就需要建立一个转换的字典
	*/
	for index, oneDialogueEx := range infoBase.DialoguesEx {
		if oneDialogueEx.EnLine == "" {
			continue
		}
		baseCorpus = append(baseCorpus, oneDialogueEx.EnLine)
		baseDialogueFilterMap[len(baseCorpus)-1] = index
	}
	// 初始化
	pipLine, tfidf, err := NewTFIDF(baseCorpus)
	if err != nil {
		return false, 0, 0, err
	}

	/*
		确认两个字幕间的偏移，暂定的方案是两边都连续匹配上 5 个索引，再抽取一个对话的时间进行修正计算
	*/
	maxCompareDialogue := s.FixerConfig.V1_MaxCompareDialogue
	// 基线的长度
	_, docsLength := tfidf.Dims()
	var matchIndexList = make([]MatchIndex, 0)
	sc := NewSubCompare(maxCompareDialogue)
	// 开始比较相似度，默认认为是 Ch_en 就行了
	for srcIndex := 0; srcIndex < len(infoSrc.DialoguesEx); {

		srcOneDialogueEx := infoSrc.DialoguesEx[srcIndex]
		// 这里只考虑 英文 的语言
		if srcOneDialogueEx.EnLine == "" {
			srcIndex++
			continue
		}
		// run the query through the same pipeline that was fitted to the corpus and
		// to project it into the same dimensional space
		queryVector, err := pipLine.Transform(srcOneDialogueEx.EnLine)
		if err != nil {
			return false, 0, 0, err
		}
		// iterate over document feature vectors (columns) in the LSI matrix and compare
		// with the query vector for similarity.  Similarity is determined by the difference
		// between the angles of the vectors known as the cosine similarity
		highestSimilarity := -1.0
		// 匹配上的基准的索引
		var baseIndex int
		// 这里理论上需要把所有的基线遍历一次，但是，一般来说，两个字幕不可能差距在 50 行
		// 这样的好处是有助于提高搜索的性能
		// 那么就以当前的 src 的位置，向前、向后各 50 来遍历
		nowMaxScanLength := srcIndex + 50
		nowMinScanLength := srcIndex - 50
		if nowMinScanLength < 0 {
			nowMinScanLength = 0
		}
		if nowMaxScanLength > docsLength {
			nowMaxScanLength = docsLength
		}
		for i := nowMinScanLength; i < nowMaxScanLength; i++ {
			similarity := pairwise.CosineSimilarity(queryVector.(mat.ColViewer).ColView(0), tfidf.(mat.ColViewer).ColView(i))
			if similarity > highestSimilarity {
				baseIndex = i
				highestSimilarity = similarity
			}
		}

		startBaseIndex, startSrcIndex := sc.GetStartIndex()
		if sc.Add(baseIndex, srcIndex) == false {
			sc.Clear()
			srcIndex = startSrcIndex + 1
			continue
			//sc.Add(baseIndex, srcIndex)
		}
		if sc.Check() == false {
			srcIndex++
			continue
		} else {
			sc.Clear()
		}

		matchIndexList = append(matchIndexList, MatchIndex{
			BaseNowIndex: startBaseIndex,
			//BaseNowIndex: baseDialogueFilterMap[startBaseIndex],
			SrcNowIndex: startSrcIndex,
			Similarity:  highestSimilarity,
		})

		//println(fmt.Sprintf("Similarity: %f Base[%d] %s-%s '%s' <--> Src[%d] %s-%s '%s'",
		//	highestSimilarity,
		//	baseIndex, infoBase.DialoguesEx[baseIndex].relativelyStartTime, infoBase.DialoguesEx[baseIndex].relativelyEndTime, baseCorpus[baseIndex],
		//	srcIndex, srcOneDialogueEx.relativelyStartTime, srcOneDialogueEx.relativelyEndTime, srcOneDialogueEx.EnLine))

		srcIndex++
	}

	var startDiffTimeLineData = make([]opts.LineData, 0)
	var endDiffTimeLineData = make([]opts.LineData, 0)
	var tmpStartDiffTime = make([]float64, 0)
	var tmpEndDiffTime = make([]float64, 0)
	var startDiffTimeList = make(stat.Float64Slice, 0)
	var endDiffTimeList = make(stat.Float64Slice, 0)
	var xAxis = make([]string, 0)
	// 上面找出了连续匹配 maxCompareDialogue：N 次的字幕语句块
	// 求出平均时间偏移
	for mIndex, matchIndexItem := range matchIndexList {

		for i := 0; i < maxCompareDialogue; i++ {
			// 这里会统计连续的这 5 句话的时间差
			//tmpBaseIndex := matchIndexItem.BaseNowIndex + i
			tmpBaseIndex := baseDialogueFilterMap[matchIndexItem.BaseNowIndex+i]
			tmpSrcIndex := matchIndexItem.SrcNowIndex + i

			baseTimeStart, err := infoBase.ParseTime(infoBase.DialoguesEx[tmpBaseIndex].StartTime)
			if err != nil {
				return false, 0, 0, err
			}
			baseTimeEnd, err := infoBase.ParseTime(infoBase.DialoguesEx[tmpBaseIndex].EndTime)
			if err != nil {
				return false, 0, 0, err
			}
			srtTimeStart, err := infoBase.ParseTime(infoSrc.DialoguesEx[tmpSrcIndex].StartTime)
			if err != nil {
				return false, 0, 0, err
			}
			srtTimeEnd, err := infoBase.ParseTime(infoSrc.DialoguesEx[tmpSrcIndex].EndTime)
			if err != nil {
				return false, 0, 0, err
			}

			TimeDiffStart := baseTimeStart.Sub(srtTimeStart)
			TimeDiffEnd := baseTimeEnd.Sub(srtTimeEnd)

			startDiffTimeLineData = append(startDiffTimeLineData, opts.LineData{Value: TimeDiffStart.Seconds()})
			endDiffTimeLineData = append(endDiffTimeLineData, opts.LineData{Value: TimeDiffEnd.Seconds()})

			tmpStartDiffTime = append(tmpStartDiffTime, TimeDiffStart.Seconds())
			tmpEndDiffTime = append(tmpEndDiffTime, TimeDiffEnd.Seconds())

			startDiffTimeList = append(startDiffTimeList, TimeDiffStart.Seconds())
			endDiffTimeList = append(endDiffTimeList, TimeDiffEnd.Seconds())

			xAxis = append(xAxis, fmt.Sprintf("%d_%d", mIndex, i))

			debugInfos = append(debugInfos, "bs "+infoBase.DialoguesEx[tmpBaseIndex].StartTime+" <-> "+infoBase.DialoguesEx[tmpBaseIndex].EndTime)
			debugInfos = append(debugInfos, "sc "+infoSrc.DialoguesEx[tmpSrcIndex].StartTime+" <-> "+infoSrc.DialoguesEx[tmpSrcIndex].EndTime)
			debugInfos = append(debugInfos, "StartDiffTime: "+fmt.Sprintf("%f", TimeDiffStart.Seconds()))
			//println(fmt.Sprintf("Diff Start-End: %s - %s Base[%d] %s-%s '%s' <--> Src[%d] %s-%s '%s'",
			//	TimeDiffStart, TimeDiffEnd,
			//	tmpBaseIndex, infoBase.DialoguesEx[tmpBaseIndex].relativelyStartTime, infoBase.DialoguesEx[tmpBaseIndex].relativelyEndTime, infoBase.DialoguesEx[tmpBaseIndex].EnLine,
			//	tmpSrcIndex, infoSrc.DialoguesEx[tmpSrcIndex].relativelyStartTime, infoSrc.DialoguesEx[tmpSrcIndex].relativelyEndTime, infoSrc.DialoguesEx[tmpSrcIndex].EnLine))
		}
		debugInfos = append(debugInfos, "---------------------------------------------")
		//println("---------------------------------------------")
	}

	oldMean := stat.Mean(startDiffTimeList)
	oldSd := stat.Sd(startDiffTimeList)
	newMean := -1.0
	newSd := -1.0
	per := 1.0

	// 如果 SD 较大的时候才需要剔除
	if oldSd > 0.1 {
		var outliersMap = make(map[float64]int, 0)
		outliers, _, _ := tukey.Outliers(0.3, tmpStartDiffTime)
		for _, outlier := range outliers {
			outliersMap[outlier] = 0
		}
		var newStartDiffTimeList = make([]float64, 0)
		for _, f := range tmpStartDiffTime {

			_, ok := outliersMap[f]
			if ok == true {
				continue
			}

			newStartDiffTimeList = append(newStartDiffTimeList, f)
		}

		orgLen := startDiffTimeList.Len()
		startDiffTimeList = make(stat.Float64Slice, 0)
		for _, f := range newStartDiffTimeList {
			startDiffTimeList = append(startDiffTimeList, f)
		}
		newLen := startDiffTimeList.Len()

		per = float64(newLen) / float64(orgLen)

		newMean = stat.Mean(startDiffTimeList)
		newSd = stat.Sd(startDiffTimeList)
	}

	if newMean == -1.0 {
		newMean = oldMean
	}
	if newSd == -1.0 {
		newSd = oldSd
	}

	// 不为空的时候，生成调试文件
	if staticLineFileSavePath != "" {
		//staticLineFileSavePath = "bar.html"
		err = SaveStaticLineV1(staticLineFileSavePath, infoBase.Name, infoSrc.Name,
			per, oldMean, oldSd, newMean, newSd, xAxis,
			startDiffTimeLineData, endDiffTimeLineData)
		if err != nil {
			return false, 0, 0, err
		}
	}

	// 跳过的逻辑是 mean 是 0 ，那么现在如果判断有问题，缓存的调试文件继续生成，然后强制返回 0 来跳过后续的逻辑
	// 这里需要考虑，找到的连续 5 句话匹配的有多少句，占比整体所有的 Dialogue 是多少，太低也需要跳过
	matchIndexLineCount := len(matchIndexList) * maxCompareDialogue
	//perMatch := float64(matchIndexLineCount) / float64(len(infoSrc.DialoguesEx))
	perMatch := float64(matchIndexLineCount) / float64(len(baseCorpus))
	if perMatch < s.FixerConfig.V1_MinMatchedPercent {
		tmpContent := infoSrc.Name + fmt.Sprintf(" Sequence match %d dialogues (< %f%%), Skip,", s.FixerConfig.V1_MaxCompareDialogue, s.FixerConfig.V1_MinMatchedPercent*100) + fmt.Sprintf(" %f%% ", perMatch*100)

		debugInfos = append(debugInfos, tmpContent)

		log_helper.GetLogger().Infoln(tmpContent)
	} else {
		tmpContent := infoSrc.Name + fmt.Sprintf(" Sequence match %d dialogues,", s.FixerConfig.V1_MaxCompareDialogue) + fmt.Sprintf(" %f%% ", perMatch*100)

		debugInfos = append(debugInfos, tmpContent)

		log_helper.GetLogger().Infoln(tmpContent)
	}

	// 输出调试的匹配时间轴信息的列表
	if debugInfoFileSavePath != "" {
		err = my_util.WriteStrings2File(debugInfoFileSavePath, debugInfos)
		if err != nil {
			return false, 0, 0, err
		}
	}
	// 虽然有条件判断是认为有问题的，但是返回值还是要填写除去的
	if perMatch < s.FixerConfig.V1_MinMatchedPercent {
		return false, newMean, newSd, nil
	}

	return true, newMean, newSd, nil
}

// GetOffsetTimeV2 使用内置的字幕校正外置的字幕时间轴
func (s *SubTimelineFixer) GetOffsetTimeV2(baseUnit, srcUnit *sub_helper.SubUnit, audioVadList []vad.VADInfo) (bool, float64, float64, error) {

	// -------------------------------------------------
	/*
		开始针对对白单元进行匹配
		下面的逻辑需要参考 FFT识别流程.jpg 这个图示来理解
		实际实现的时候，会在上述 srcUnit 上，做一个滑动窗口来做匹配，80% 是窗口，20% 用于移动
		步长固定在 10 步
	*/
	audioFloatList := vad.GetFloatSlice(audioVadList)

	srcVADLen := len(srcUnit.VADList)
	// 滑动窗口的长度
	srcWindowLen := int(float64(srcVADLen) * s.FixerConfig.V2_WindowMatchPer)
	// 划分为 4 个区域，每一个部分的长度
	const parts = 20
	perPartLen := srcVADLen / parts
	matchedInfos := make([]MatchInfo, 0)

	subVADBlockInfos := make([]SubVADBlockInfo, 0)
	for i := 0; i < parts; i++ {

		// 滑动窗体的起始 Index
		srcSlideStartIndex := i * perPartLen
		// 滑动的距离
		srcSlideLen := perPartLen
		// 一步的长度
		oneStep := perPartLen / s.FixerConfig.V2_CompareParts
		if srcSlideLen <= 0 {
			srcSlideLen = 1
		}
		if oneStep <= 0 {
			oneStep = 1
		}
		// -------------------------------------------------
		windowInfo := WindowInfo{
			BaseAudioFloatList: audioFloatList,
			BaseUnit:           baseUnit,
			SrcUnit:            srcUnit,
			MatchedTimes:       0,
			SrcWindowLen:       srcWindowLen,
			SrcSlideStartIndex: srcSlideStartIndex,
			SrcSlideLen:        srcSlideLen,
			OneStep:            oneStep,
		}
		subVADBlockInfos = append(subVADBlockInfos, SubVADBlockInfo{
			Index:      i,
			StartIndex: srcSlideStartIndex,
			EndIndex:   srcSlideStartIndex + srcSlideLen,
		})
		// 实际 FFT 的匹配逻辑函数
		// 时间轴差值数组
		matchInfo, err := s.slidingWindowProcessor(&windowInfo)
		if err != nil {
			return false, 0, 0, err
		}

		matchedInfos = append(matchedInfos, *matchInfo)
	}

	// 这里可能遇到匹配的时候没有能够执行够 V2_CompareParts 次，有可能是负数跳过或者时间转换失败导致，前者为主（可能是这两个就是一个东西的时候，或者说没有时间轴偏移的时候）
	//if len(matchInfo.StartDiffTimeList) < s.FixerConfig.V2_CompareParts/2 {
	//	log_helper.GetLogger().Infoln("Can't Match, Parts=", len(matchInfo.StartDiffTimeList), "At Least", s.FixerConfig.V2_CompareParts/2)
	//	return false, 0, 0, nil
	//}

	for index, matchInfo := range matchedInfos {

		log_helper.GetLogger().Infoln("------------------------------------")
		outCorrelationFixResult := s.calcMeanAndSD(matchInfo.StartDiffTimeListEx, matchInfo.StartDiffTimeList)
		log_helper.GetLogger().Infoln(fmt.Sprintf("FFTAligner Old Mean: %v SD: %f Per: %v", outCorrelationFixResult.OldMean, outCorrelationFixResult.OldSD, outCorrelationFixResult.Per))
		log_helper.GetLogger().Infoln(fmt.Sprintf("FFTAligner New Mean: %v SD: %f Per: %v", outCorrelationFixResult.NewMean, outCorrelationFixResult.NewSD, outCorrelationFixResult.Per))

		value, indexMax := matchInfo.StartDiffTimeMap.Max()
		log_helper.GetLogger().Infoln("FFTAligner Max score:", fmt.Sprintf("%v", value.(float64)), "Time:", fmt.Sprintf("%v", matchInfo.StartDiffTimeList[indexMax.(int)]))

		/*
			如果 outCorrelationFixResult 的 SD > 0.1，那么大概率这个时间轴的值匹配的有问题，需要向左或者向右找一个值进行继承
			-4 0.001
			-4 0.001
			-4 0.001
			-200 0.1
			-4 0.001
			比如这种情况，那么就需要向左找到 -4 去继承。
			具体的实现：
				找到一个 SD > 0.1 的项目，那么就需要从左边和右边同时对比
				首先是他们的差值要在 1s （绝对值）以内，优先往左边找，如果绝对值成立则判断 SD （SD 必须 < 0.1）
				如果只是 SD 不成立，那么就继续往左，继续判断差值和 SD。如果都找不到合适的，就要回到”起点“，从右开始找，逻辑一样
				直到没有找到合适的信息，就报错
		*/
		if outCorrelationFixResult.NewSD < 0.1 {
			continue
		}
		// 是否找到合适的继承值
		bProcess := false
		// 先往左
		if index-1 >= 0 {
			// 说明至少可以往左
		}
	}

	return true, 0, 0, nil
}

func (s SubTimelineFixer) fixOnePart() {

}

// slidingWindowProcessor 滑动窗口计算时间轴偏移
func (s *SubTimelineFixer) slidingWindowProcessor(windowInfo *WindowInfo) (*MatchInfo, error) {

	// -------------------------------------------------
	var bUseSubOrAudioAsBase = true
	if windowInfo.BaseUnit == nil && windowInfo.BaseAudioFloatList != nil {
		// 使用 音频 来进行匹配
		bUseSubOrAudioAsBase = false
	} else if windowInfo.BaseUnit != nil {
		// 使用 字幕 来进行匹配
		bUseSubOrAudioAsBase = true
	} else {
		return nil, errors.New("GetOffsetTimeV2 input baseUnit or AudioVad is nil")
	}
	// -------------------------------------------------
	outMatchInfo := MatchInfo{
		StartDiffTimeList:   make([]float64, 0),
		StartDiffTimeMap:    treemap.NewWith(utils.Float64Comparator),
		StartDiffTimeListEx: make(stat.Float64Slice, 0),
	}
	fixFunc := func(i interface{}) error {
		inData := i.(InputData)
		// -------------------------------------------------
		// 开始匹配
		// 这里的对白单元，当前的 Base 进行对比，详细示例见图解。Step 2 中橙色的区域
		fffAligner := NewFFTAligner()
		var bok = false
		var nowBaseStartTime = 0.0
		var offsetIndex = 0
		var score = 0.0
		// 图解，参考 Step 3
		if bUseSubOrAudioAsBase == false {
			// 使用 音频 来进行匹配
			// 去掉头和尾，具体百分之多少，见 V2_FrontAndEndPerBase
			audioCutLen := int(float64(len(inData.BaseAudioVADList)) * s.FixerConfig.V2_FrontAndEndPerBase)

			srcMaxLen := windowInfo.SrcWindowLen + inData.OffsetIndex
			if srcMaxLen >= len(inData.SrcUnit.GetVADFloatSlice()) {
				srcMaxLen = len(inData.SrcUnit.GetVADFloatSlice()) - 1
			}
			offsetIndex, score = fffAligner.Fit(inData.BaseAudioVADList[audioCutLen:len(inData.BaseAudioVADList)-audioCutLen], inData.SrcUnit.GetVADFloatSlice()[inData.OffsetIndex:srcMaxLen])
			realOffsetIndex := offsetIndex + audioCutLen
			if realOffsetIndex < 0 {
				return nil
			}
			// offsetIndex 这里得到的是 10ms 为一个单位的 OffsetIndex
			nowBaseStartTime = vad.GetAudioIndex2Time(realOffsetIndex)

		} else {
			// 使用 字幕 来进行匹配

			srcMaxLen := inData.OffsetIndex + windowInfo.SrcWindowLen
			if srcMaxLen >= len(inData.SrcUnit.GetVADFloatSlice()) {
				srcMaxLen = len(inData.SrcUnit.GetVADFloatSlice()) - 1
			}
			offsetIndex, score = fffAligner.Fit(inData.BaseUnit.GetVADFloatSlice(), inData.SrcUnit.GetVADFloatSlice()[inData.OffsetIndex:srcMaxLen])
			if offsetIndex < 0 {
				return nil
			}
			bok, nowBaseStartTime = inData.BaseUnit.GetIndexTimeNumber(offsetIndex, true)
			if bok == false {
				return nil
			}
		}
		// 需要校正的字幕
		bok, nowSrcStartTime := inData.SrcUnit.GetIndexTimeNumber(inData.OffsetIndex, true)
		if bok == false {
			return nil
		}
		// 时间差值
		TimeDiffStartCorrelation := nowBaseStartTime - nowSrcStartTime
		log_helper.GetLogger().Debugln("------------")
		log_helper.GetLogger().Debugln("OffsetTime:", fmt.Sprintf("%v", TimeDiffStartCorrelation),
			"offsetIndex:", offsetIndex,
			"score:", fmt.Sprintf("%v", score))

		mutexFixV2.Lock()
		outMatchInfo.StartDiffTimeList = append(outMatchInfo.StartDiffTimeList, TimeDiffStartCorrelation)
		outMatchInfo.StartDiffTimeListEx = append(outMatchInfo.StartDiffTimeListEx, TimeDiffStartCorrelation)
		outMatchInfo.StartDiffTimeMap.Put(score, windowInfo.MatchedTimes)
		windowInfo.MatchedTimes++
		mutexFixV2.Unlock()
		// -------------------------------------------------
		return nil
	}
	// -------------------------------------------------
	antPool, err := ants.NewPoolWithFunc(s.FixerConfig.V2_FixThreads, func(inData interface{}) {
		data := inData.(InputData)
		defer data.Wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(s.FixerConfig.V2_SubOneUnitProcessTimeOut)*time.Second)
		defer cancel()

		done := make(chan error, 1)
		panicChan := make(chan interface{}, 1)
		go func() {
			defer func() {
				if p := recover(); p != nil {
					panicChan <- p
				}
			}()

			done <- fixFunc(inData)
		}()

		select {
		case err := <-done:
			if err != nil {
				log_helper.GetLogger().Errorln("GetOffsetTimeV2.NewPoolWithFunc done with Error", err.Error())
			}
			return
		case p := <-panicChan:
			log_helper.GetLogger().Errorln("GetOffsetTimeV2.NewPoolWithFunc got panic", p)
			return
		case <-ctx.Done():
			log_helper.GetLogger().Errorln("GetOffsetTimeV2.NewPoolWithFunc got time out", ctx.Err())
			return
		}
	})
	if err != nil {
		return nil, err
	}
	defer antPool.Release()
	// -------------------------------------------------
	wg := sync.WaitGroup{}
	for i := windowInfo.SrcSlideStartIndex; i < windowInfo.SrcSlideStartIndex+windowInfo.SrcSlideLen-1; {
		wg.Add(1)

		if bUseSubOrAudioAsBase == true {
			// 使用字幕
			err = antPool.Invoke(InputData{BaseUnit: *windowInfo.BaseUnit, SrcUnit: *windowInfo.SrcUnit, OffsetIndex: i, Wg: &wg})
		} else {
			// 使用音频
			err = antPool.Invoke(InputData{BaseAudioVADList: windowInfo.BaseAudioFloatList, SrcUnit: *windowInfo.SrcUnit, OffsetIndex: i, Wg: &wg})
		}

		if err != nil {
			log_helper.GetLogger().Errorln("GetOffsetTimeV2 ants.Invoke", err)
		}

		i += windowInfo.OneStep
	}
	wg.Wait()

	return &outMatchInfo, nil
}

func (s *SubTimelineFixer) calcMeanAndSD(startDiffTimeList stat.Float64Slice, tmpStartDiffTime []float64) FixResult {

	oldMean := stat.Mean(startDiffTimeList)
	oldSd := stat.Sd(startDiffTimeList)
	newMean := MinValue
	newSd := MinValue
	per := 1.0

	if len(tmpStartDiffTime) < 3 {
		return FixResult{
			oldMean,
			oldSd,
			oldMean,
			oldSd,
			per,
		}
	}

	// 如果 SD 较大的时候才需要剔除
	if oldSd > 0.1 {
		var outliersMap = make(map[float64]int, 0)
		outliers, _, _ := tukey.Outliers(0.3, tmpStartDiffTime)
		for _, outlier := range outliers {
			outliersMap[outlier] = 0
		}
		var newStartDiffTimeList = make([]float64, 0)
		for _, f := range tmpStartDiffTime {

			_, ok := outliersMap[f]
			if ok == true {
				continue
			}

			newStartDiffTimeList = append(newStartDiffTimeList, f)
		}

		orgLen := startDiffTimeList.Len()
		startDiffTimeList = make(stat.Float64Slice, 0)
		for _, f := range newStartDiffTimeList {
			startDiffTimeList = append(startDiffTimeList, f)
		}
		newLen := startDiffTimeList.Len()

		per = float64(newLen) / float64(orgLen)

		newMean = stat.Mean(startDiffTimeList)
		newSd = stat.Sd(startDiffTimeList)
	}

	if my_util.IsEqual(newMean, MinValue) == true {
		newMean = oldMean
	}
	if my_util.IsEqual(newSd, MinValue) == true {
		newSd = oldSd
	}
	return FixResult{
		oldMean,
		oldSd,
		newMean,
		newSd,
		per,
	}
}

const FixMask = "-fix"
const MinValue = -9999.0

var mutexFixV2 sync.Mutex

// MatchInfo 匹配的信息
type MatchInfo struct {
	StartDiffTimeList   []float64
	StartDiffTimeMap    *treemap.Map
	StartDiffTimeListEx stat.Float64Slice
}

// WindowInfo 滑动窗体信息
type WindowInfo struct {
	BaseAudioFloatList []float64           // 基准 VAD
	BaseUnit           *sub_helper.SubUnit // 基准 VAD
	SrcUnit            *sub_helper.SubUnit // 需要匹配的 VAD
	MatchedTimes       int                 // 匹配上的次数
	SrcWindowLen       int                 // 滑动窗体长度
	SrcSlideStartIndex int                 // 滑动起始索引
	SrcSlideLen        int                 // 滑动距离
	OneStep            int                 // 每次滑动的长度
}

// InputData 修复函数传入多线程的数据结构
type InputData struct {
	BaseUnit         sub_helper.SubUnit // 基准 VAD
	BaseAudioVADList []float64          // 基准 VAD
	SrcUnit          sub_helper.SubUnit // 需要匹配的 VAD
	OffsetIndex      int                // 滑动窗体的移动偏移索引
	Wg               *sync.WaitGroup    // 并发锁
}

// SubVADBlockInfo 字幕分块信息
type SubVADBlockInfo struct {
	Index      int
	StartIndex int
	EndIndex   int
}
