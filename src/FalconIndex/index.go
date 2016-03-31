/*****************************************************************************
 *  file name : index.go
 *  author : Wu Yinghao
 *  email  : wyh817@gmail.com
 *
 *  file description : 索引的源代码，可以完成一次完备的查询
 *
******************************************************************************/

package FalconIndex

import (
	fis "FalconIndex/segment"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"tree"
	"utils"
)

type Index struct {
	Name          string                           `json:"name"`
	Pathname      string                           `json:"pathname"`
	Fields        map[string]utils.SimpleFieldInfo `json:"fields"`
	PrimaryKey    string                           `json:"primarykey"`
	StartDocId    uint32                           `json:"startdocid"`
	MaxDocId      uint32                           `json:"maxdocid"`
	PrefixSegment uint64                           `json:"prefixsegment"`
	SegmentNames  []string                         `json:"segmentnames"`

	segments        []*fis.Segment //磁盘的段
	memorySegment   *fis.Segment   //内存段
	primary         *tree.BTreedb  //主键单独用B+树保存
	bitmap          *utils.Bitmap  //bitmap用来删除数据
	dict            *tree.BTreedb  //字典，保存DF信息，暂时无用
	fieldnames      []string
	idxSegmentMutex *sync.Mutex   //段锁，当段序列化到磁盘或者段合并时使用或者新建段时使用
	Logger          *utils.Log4FE `json:"-"`
}

// NewEmptyIndex function description : 新建空索引
// params : name:索引名称
//          pathname:路径名称
// return : 索引实例
func NewEmptyIndex(name, pathname string, logger *utils.Log4FE) *Index {

	this := &Index{Name: name, Logger: logger, StartDocId: 0, MaxDocId: 0, PrefixSegment: 1000,
		SegmentNames: make([]string, 0), PrimaryKey: "", segments: make([]*fis.Segment, 0),
		memorySegment: nil, primary: nil, bitmap: nil, Pathname: pathname,
		Fields: make(map[string]utils.SimpleFieldInfo), idxSegmentMutex: new(sync.Mutex),
		dict: nil, fieldnames: make([]string, 0)}

	bitmapname := fmt.Sprintf("%v%v.bitmap", pathname, name)
	utils.MakeBitmapFile(bitmapname)
	this.bitmap = utils.NewBitmap(bitmapname)

	dictfilename := fmt.Sprintf("%v%v_dict.dic", this.Pathname, this.Name)
	this.dict = tree.NewBTDB(dictfilename)

	primaryname := fmt.Sprintf("%v%v_primary.pk", this.Pathname, this.Name)
	this.primary = tree.NewBTDB(primaryname)
	this.primary.AddBTree(utils.DEFAULT_PRIMARY_KEY)
	this.PrimaryKey = utils.DEFAULT_PRIMARY_KEY

	return this
}

// NewIndexWithLocalFile function description : 从磁盘启动索引
// params : name:索引名称
//          pathname:路径名称
// return : 索引实例
func NewIndexWithLocalFile(name, pathname string, logger *utils.Log4FE) *Index {
	this := &Index{Name: name, Logger: logger, StartDocId: 0, MaxDocId: 0, PrefixSegment: 1000,
		SegmentNames: make([]string, 0), PrimaryKey: "", segments: make([]*fis.Segment, 0),
		memorySegment: nil, primary: nil, bitmap: nil, Pathname: pathname,
		Fields: make(map[string]utils.SimpleFieldInfo), idxSegmentMutex: new(sync.Mutex),
		dict: nil, fieldnames: make([]string, 0)}

	metaFileName := fmt.Sprintf("%v%v.meta", pathname, name)
	buffer, err := utils.ReadFromJson(metaFileName)
	if err != nil {
		return this
	}

	err = json.Unmarshal(buffer, &this)
	if err != nil {
		return this
	}

	dictfilename := fmt.Sprintf("%v%v_dict.dic", this.Pathname, this.Name)
	if utils.Exist(dictfilename) {
		this.Logger.Info("[INFO] Load dictfilename %v", dictfilename)
		this.dict = tree.NewBTDB(dictfilename)
	}

	for _, segmentname := range this.SegmentNames {
		segment := fis.NewSegmentWithLocalFile(segmentname, this.dict, logger)
		this.segments = append(this.segments, segment)

	}

	//新建空的段
	segmentname := fmt.Sprintf("%v%v_%v", this.Pathname, this.Name, this.PrefixSegment)
	var fields []utils.SimpleFieldInfo
	for _, f := range this.Fields {
		if f.FieldType != utils.IDX_TYPE_PK {
			fields = append(fields, f)
			this.fieldnames = append(this.fieldnames, f.FieldName)
		}

	}

	this.memorySegment = fis.NewEmptySegmentWithFieldsInfo(segmentname, this.MaxDocId, fields, this.dict, this.Logger)
	this.PrefixSegment++

	//读取bitmap
	bitmapname := fmt.Sprintf("%v%v.bitmap", pathname, name)
	this.bitmap = utils.NewBitmap(bitmapname)

	//if this.PrimaryKey != "" {
	primaryname := fmt.Sprintf("%v%v_primary.pk", this.Pathname, this.Name)
	this.primary = tree.NewBTDB(primaryname)
	//}

	this.Logger.Info("[INFO] Load Index %v success", this.Name)
	return this

}

// AddField function description : 新增字段
// params : field 字段信息
// return :
func (this *Index) AddField(field utils.SimpleFieldInfo) error {

	if _, ok := this.Fields[field.FieldName]; ok {
		this.Logger.Warn("[WARN] field %v Exist ", field.FieldName)
		return nil
	}

	this.Fields[field.FieldName] = field
	this.fieldnames = append(this.fieldnames, field.FieldName)
	if field.FieldType == utils.IDX_TYPE_STRING_SEG {
		this.dict.AddBTree(field.FieldName)
	}
	if field.FieldType == utils.IDX_TYPE_PK {
		this.PrimaryKey = field.FieldName
		//primaryname := fmt.Sprintf("%v%v_primary.pk", this.Pathname, this.Name)
		//this.primary = tree.NewBTDB(primaryname)
		this.primary.AddBTree(field.FieldName)
	} else {
		this.idxSegmentMutex.Lock()
		defer this.idxSegmentMutex.Unlock()

		if this.memorySegment == nil {
			segmentname := fmt.Sprintf("%v%v_%v", this.Pathname, this.Name, this.PrefixSegment)
			var fields []utils.SimpleFieldInfo
			for _, f := range this.Fields {
				if f.FieldType != utils.IDX_TYPE_PK {
					fields = append(fields, f)
				}

			}
			this.memorySegment = fis.NewEmptySegmentWithFieldsInfo(segmentname, this.MaxDocId, fields, this.dict, this.Logger)
			this.PrefixSegment++

		} else if this.memorySegment.IsEmpty() {
			err := this.memorySegment.AddField(field)
			if err != nil {
				this.Logger.Error("[ERROR] Add Field Error  %v", err)
				return err
			}
		} else {
			tmpsegment := this.memorySegment
			if err := tmpsegment.Serialization(); err != nil {
				return err
			}
			this.segments = append(this.segments, tmpsegment)
			this.SegmentNames = make([]string, 0)
			for _, seg := range this.segments {
				this.SegmentNames = append(this.SegmentNames, seg.SegmentName)
			}

			segmentname := fmt.Sprintf("%v%v_%v", this.Pathname, this.Name, this.PrefixSegment)
			var fields []utils.SimpleFieldInfo
			for _, f := range this.Fields {
				if f.FieldType != utils.IDX_TYPE_PK {
					fields = append(fields, f)
				}

			}
			this.memorySegment = fis.NewEmptySegmentWithFieldsInfo(segmentname, this.MaxDocId, fields, this.dict, this.Logger)
			this.PrefixSegment++

		}

	}
	return this.storeStruct()
}

// DeleteField function description : 删除字段
// params : 字段名称
// return :
func (this *Index) DeleteField(fieldname string) error {

	if _, ok := this.Fields[fieldname]; !ok {
		this.Logger.Warn("[WARN] field %v not found ", fieldname)
		return nil
	}

	if fieldname == this.PrimaryKey {
		this.Logger.Warn("[WARN] field %v is primary key can not delete ", fieldname)
		return nil
	}

	this.idxSegmentMutex.Lock()
	defer this.idxSegmentMutex.Unlock()

	if this.memorySegment == nil {
		this.memorySegment.DeleteField(fieldname)
		delete(this.Fields, fieldname)
		return this.storeStruct()
	}

	if this.memorySegment.IsEmpty() {
		this.memorySegment.DeleteField(fieldname)
		delete(this.Fields, fieldname)
		return this.storeStruct()
	}

	delete(this.Fields, fieldname)

	tmpsegment := this.memorySegment
	if err := tmpsegment.Serialization(); err != nil {
		return err
	}
	this.segments = append(this.segments, tmpsegment)
	this.SegmentNames = make([]string, 0)
	for _, seg := range this.segments {
		this.SegmentNames = append(this.SegmentNames, seg.SegmentName)
	}

	segmentname := fmt.Sprintf("%v%v_%v", this.Pathname, this.Name, this.PrefixSegment)
	var fields []utils.SimpleFieldInfo
	for _, f := range this.Fields {
		if f.FieldType != utils.IDX_TYPE_PK {
			fields = append(fields, f)
		}

	}
	this.memorySegment = fis.NewEmptySegmentWithFieldsInfo(segmentname, this.MaxDocId, fields, this.dict, this.Logger)
	this.PrefixSegment++

	return this.storeStruct()

}

// storeStruct function description : 存储索引元信息，json格式
// params :
// return :
func (this *Index) storeStruct() error {
	metaFileName := fmt.Sprintf("%v%v.meta", this.Pathname, this.Name)
	if err := utils.WriteToJson(this, metaFileName); err != nil {
		return err
	}
	return nil

}

// UpdateDocument function description : 更新文档，新增文档
// params :
// return :
func (this *Index) UpdateDocument(content map[string]string) error {

	if len(this.Fields) == 0 {
		this.Logger.Error("[ERROR] No Field or Segment is nil")
		return errors.New("no field or segment is nil")
	}

	if this.memorySegment == nil {
		this.idxSegmentMutex.Lock()
		segmentname := fmt.Sprintf("%v%v_%v", this.Pathname, this.Name, this.PrefixSegment)
		var fields []utils.SimpleFieldInfo
		for _, f := range this.Fields {
			if f.FieldType != utils.IDX_TYPE_PK {
				fields = append(fields, f)
			}

		}
		this.memorySegment = fis.NewEmptySegmentWithFieldsInfo(segmentname, this.MaxDocId, fields, this.dict, this.Logger)
		this.PrefixSegment++
		if err := this.storeStruct(); err != nil {
			this.idxSegmentMutex.Unlock()
			return err
		}
		this.idxSegmentMutex.Unlock()
	}

	docid := this.MaxDocId
	this.MaxDocId++

	//无主键的表直接添加
	if this.PrimaryKey == utils.DEFAULT_PRIMARY_KEY {
		uuid, _ := utils.NewV4()
		//uuid := fmt.Sprintf("%v",buuid)
		//this.Logger.Info("[INFO] UUID :: %v",uuid.String())
		if err := this.primary.Set(utils.DEFAULT_PRIMARY_KEY, uuid.String(), uint64(docid)); err != nil {
			this.MaxDocId--
			return err
		}
		return this.memorySegment.AddDocument(docid, content)
	}

	if _, hasPrimary := content[this.PrimaryKey]; !hasPrimary {
		this.Logger.Error("[ERROR] Primary Key Not Found %v", this.PrimaryKey)
		this.MaxDocId--
		return errors.New("No Primary Key")
	}

	if err := this.updatePrimaryKey(content[this.PrimaryKey], docid); err != nil {
		this.MaxDocId--
		return err
	}

	return this.memorySegment.AddDocument(docid, content)

}

// updatePrimaryKey function description : 更新主键对应的docid
// params : 主键，docid
// return :
func (this *Index) updatePrimaryKey(key string, docid uint32) error {

	err := this.primary.Set(this.PrimaryKey, key, uint64(docid))

	if err != nil {
		this.Logger.Error("[ERROR] update Put key error  %v", err)
		return err
	}

	return nil
}

// findPrimaryKey function description : 查找主键
// params : 主键
// return : docid，是否找到
func (this *Index) findPrimaryKey(key string) (utils.DocIdNode, bool) {

	ok, val := this.primary.Search(this.PrimaryKey, key)
	if !ok || val >= uint64(this.memorySegment.StartDocId) {
		return utils.DocIdNode{}, false
	}
	return utils.DocIdNode{Docid: uint32(val)}, true

}

// SyncMemorySegment function description : 将内存的段同步到磁盘中
// params :
// return :
func (this *Index) SyncMemorySegment() error {

	if this.memorySegment == nil {
		return nil
	}
	this.idxSegmentMutex.Lock()
	defer this.idxSegmentMutex.Unlock()

	if err := this.memorySegment.Serialization(); err != nil {
		this.Logger.Error("[ERROR] SyncMemorySegment Error %v", err)
		return err
	}
	segmentname := this.memorySegment.SegmentName
	this.memorySegment.Close()
	this.memorySegment = nil
	newSegment := fis.NewSegmentWithLocalFile(segmentname, this.dict, this.Logger)
	this.segments = append(this.segments, newSegment)
	this.SegmentNames = append(this.SegmentNames, segmentname)

	return this.storeStruct()

}

// checkMerge function description : 检查是否需要进行merge
// params :
// return :
func (this *Index) checkMerge() (int, int, bool) {
	var start int = -1
	var end int = -1
	docLens := make([]uint32, 0)
	for _, sg := range this.segments {
		docLens = append(docLens, sg.MaxDocId-sg.StartDocId)
	}

	return start, end, false

}

// MergeSegments function description : 合并段
// params :
// return :
func (this *Index) MergeSegments() error {

	var startIdx int = -1
	this.idxSegmentMutex.Lock()
	defer this.idxSegmentMutex.Unlock()
	for idx := range this.segments {
		if this.segments[idx].MaxDocId-this.segments[idx].StartDocId < 1000000 {
			startIdx = idx
			break
		}
	}
	if startIdx == -1 {
		return nil
	}

	mergeSegments := this.segments[startIdx:]

	segmentname := fmt.Sprintf("%v%v_%v", this.Pathname, this.Name, this.PrefixSegment)
	var fields []utils.SimpleFieldInfo
	for _, f := range this.Fields {
		if f.FieldType != utils.IDX_TYPE_PK {
			fields = append(fields, f)
		}

	}
	tmpSegment := fis.NewEmptySegmentWithFieldsInfo(segmentname, mergeSegments[0].StartDocId, fields, this.dict, this.Logger)
	this.PrefixSegment++
	if err := this.storeStruct(); err != nil {
		return err
	}
	tmpSegment.MergeSegments(mergeSegments)
	//tmpname:=tmpSegment.SegmentName
	tmpSegment.Close()
	tmpSegment = nil

	for _, sg := range mergeSegments {
		sg.Destroy()
	}

	tmpSegment = fis.NewSegmentWithLocalFile(segmentname, this.dict, this.Logger)
	if startIdx > 0 {
		this.segments = this.segments[:startIdx]         //make([]*fis.Segment,0)
		this.SegmentNames = this.SegmentNames[:startIdx] //make([]string,0)
	} else {
		this.segments = make([]*fis.Segment, 0)
		this.SegmentNames = make([]string, 0)
	}

	this.segments = append(this.segments, tmpSegment)
	this.SegmentNames = append(this.SegmentNames, segmentname)
	return this.storeStruct()

}

// GetFields function description : 获取所有字段名称
// params :
// return :
func (this *Index) GetFields() []string {
	return this.fieldnames
}

// GetDocumentWithFields function description : 根据字段获取docid的详情
// params : docid，字段列表
// return : docid详情，是否找到
func (this *Index) GetDocumentWithFields(docid uint32, fields []string) (map[string]string, bool) {

	for _, segment := range this.segments {
		if docid >= segment.StartDocId && docid < segment.MaxDocId {
			return segment.GetValueWithFields(docid, fields)
			//res["DocID"]=fmt.Sprintf("%d",docid)
			//return res,ok
		}
	}
	return nil, false

}

// GetDocument function description : 获取docid所有字段的信息
// params : docid
// return : docid详情，是否找到
func (this *Index) GetDocument(docid uint32) (map[string]string, bool) {

	for _, segment := range this.segments {
		if docid >= segment.StartDocId && docid < segment.MaxDocId {
			return segment.GetDocument(docid)
		}
	}
	return nil, false
}

// SearchDocIds function description : 标准查询接口
// params : 查询结构体，过滤结构体
// return :
func (this *Index) SearchDocIds(querys []utils.FSSearchQuery, filteds []utils.FSSearchFilted) ([]utils.DocIdNode, bool) {

	var ok bool
	docids := <-utils.GetDocIDsChan
	if len(querys) >= 1 {
		for _, segment := range this.segments {
			docids, _ = segment.SearchDocIds(querys[0], filteds, this.bitmap, docids)
		}
		//this.Logger.Info("[INFO] key[%v] doclens:%v",querys[0].Value,len(docids))
		docids = utils.ComputeWeight(docids, len(docids), this.MaxDocId)
	}

	if len(querys) == 1 {
		//sort.Sort(utils.DocWeightSort(docids))
		return docids, true
	}

	for _, query := range querys[1:] {

		subdocids := <-utils.GetDocIDsChan
		for _, segment := range this.segments {
			subdocids, _ = segment.SearchDocIds(query, filteds, this.bitmap, subdocids)
		}

		//this.Logger.Info("[INFO] key[%v] doclens:%v",query.Value,len(subdocids))
		docids, ok = utils.InteractionWithStartAndDf(docids, subdocids, 0, len(subdocids), this.MaxDocId)
		utils.GiveDocIDsChan <- subdocids
		if !ok {
			utils.GiveDocIDsChan <- docids
			return nil, false
		}

	}

	return docids, true

}


// GatherFields function description : 汇总字段，根据字段名称和字段的值进行汇总统计【性能堪忧】TODO
// params : docid列表，需要汇总的字段
// return :
func (this *Index) GatherFieldsByStruct(docids []utils.DocIdNode, gater utils.FSSearchGather) map[string]map[string]int {
    
    return this.GatherFields(docids,gater.FieldNames)
}

// GatherFields function description : 汇总字段，根据字段名称和字段的值进行汇总统计【性能堪忧】TODO
// params : docid列表，需要汇总的字段
// return :
func (this *Index) GatherFields(docids []utils.DocIdNode, gaters []string) map[string]map[string]int {

	gaterMap := make(map[string]map[string]int)
	for _, g := range gaters {
		gaterMap[g] = make(map[string]int)
	}

	for _, docid := range docids {

		res, _ := this.GetDocumentWithFields(docid.Docid, gaters)
		for k, v := range res {
			t := gaterMap[k]
			if _, ok := t[v]; !ok {
				t[v] = 1
			} else {
				t[v] = t[v] + 1
			}
			gaterMap[k] = t
		}

	}

	return gaterMap
}


func (this *Index) GetFieldType(fieldname string) (uint64,bool) {
    
    if _,ok:=this.Fields[fieldname];!ok{
        return 0,false
    }
    
    return this.Fields[fieldname].FieldType,true
    
}