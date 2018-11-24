package engine

import (
	"fmt"
	"log"
	"reflect"
	"sort"
	"sync"
	"time"
)

type Table interface {
	TableName() string
}

func CreateTable(obj interface{}) {
	val := reflect.ValueOf(obj)
	typ := reflect.Indirect(val).Type()
	tableName := typ.Name()

	if val.Kind() != reflect.Ptr {
		panic(fmt.Errorf("cannot use non-ptr struct %s", tableName))
	}

	log.Printf("tableName is %s", tableName)

	db.Lock()
	defer db.Unlock()

	if _, ok := db.tables[tableName]; ok {
		panic(fmt.Errorf("%s has been created", tableName))
	}

	db.tables[tableName] = make([]interface{}, 0) //store array
	db.chans[tableName] = make(chan int, ROWSIZE)
	db.rows[tableName] = make(map[int]int) //rid  -> array index
	db.locks[tableName] = &sync.Mutex{}
	db.sorting[tableName] = make(map[string]int32)
	db.sortlocks[tableName] = &sync.Mutex{}
	db.indexs[tableName] = make(map[string][]int)
	db.versions[tableName] = make(map[int]*Version)
}

////////////////////////////内部调用////////////////////////////////////////////////////////
func get(tableName string) int {
	//check table exist and lock outer
	select {
	case index := <-db.chans[tableName]:
		return index
	default:
		allocSize := len(db.tables[tableName])
		toAppend := make([]interface{}, ROWSIZE/2)
		db.tables[tableName] = append(db.tables[tableName], toAppend...)
		for i := 0; i < ROWSIZE/2; i++ {
			db.chans[tableName] <- allocSize + i
		}
		return <-db.chans[tableName]
	}
}

func put(tableName string, rid int) {
	//check table exist and lock outer
	select {
	case db.chans[tableName] <- rid:
		return
	default:
		log.Printf("table %s's chan is full", tableName)
	}
}

func sortIndex(tableName string, index string) {
	slock := db.sortlocks[tableName]
	slock.Lock()
	if v, ok := db.sorting[tableName][index]; ok && v == 1 {
		slock.Unlock()
		return
	}
	db.sorting[tableName][index] = 1
	slock.Unlock()

	time.AfterFunc(2*time.Second, func() {
		slock := db.sortlocks[tableName]
		slock.Lock()
		db.sorting[tableName][index] = 0
		slock.Unlock()

		start := time.Now().Unix()

		lock := db.locks[tableName]
		lock.Lock()
		length := len(db.indexs[tableName][index])
		sort.IntSlice(db.indexs[tableName][index]).Sort()
		lock.Unlock()

		end := time.Now().Unix()
		log.Printf("sort index %s:%s %d records finished in %d second", tableName, index, length, end-start)

	})
}

////////////////////////////////////////////////////////////////////////////////////
func Insert(obj interface{}, load ...interface{}) error {
	val := reflect.ValueOf(obj)
	typ := reflect.Indirect(val).Type()
	tableName := typ.Name()

	db.RLock()
	if _, ok := db.tables[tableName]; !ok {
		panic(fmt.Errorf("table %s is not exsit", tableName))
	}
	db.RUnlock()

	id := obj.(Row).GetID()

	lock := db.locks[tableName]
	lock.Lock()
	defer lock.Unlock()

	if _, ok := db.rows[tableName][id]; ok { //exist
		rid := db.rows[tableName][id]
		log.Printf("record id[%d] is exist in table %s %d row", id, tableName, rid)
		return fmt.Errorf("record %d is exist in %s", id, tableName)
	}

	rid := get(tableName)

	db.tables[tableName][rid] = obj
	db.rows[tableName][id] = rid

	//更新versions
	ver := &Version{Version: 1, UpdateStamp: time.Now().Unix(), SavedVersion: 0}
	////从数据库加载时load需传值，避免回写
	if len(load) != 0 {
		ver.SavedVersion = 1
	}
	db.versions[tableName][id] = ver

	//发起持久化指令
	putTrx(&Transaction{Cmd: "INSERT", TableName: tableName, ID: id, Version: (db.versions[tableName][id]).Version})

	//添加到主键索引
	pk := PRIMARYKEY
	db.indexs[tableName][pk] = append(db.indexs[tableName][pk], id)
	//索引排序
	sortIndex(tableName, pk)

	//log.Printf("insert record id[%d] in table %s's %d row", id, tableName, rid)

	indexs := obj.(Row).Index()
	if indexs == nil {
		return nil
	}

	//存在索引，创建索引
	for i := 0; i < len(indexs); i++ {
		if len(indexs[i]) == 0 {
			continue
		}
		pk := tableName
		sort.StringSlice(indexs[i]).Sort()
		for j := 0; j < len(indexs[i]); j++ {
			pk += fmt.Sprintf(":%s:%v", indexs[i][j], reflect.Indirect(val).FieldByName(indexs[i][j]))
		}
		db.indexs[tableName][pk] = append(db.indexs[tableName][pk], id)
		//索引排序
		sortIndex(tableName, pk)
	}
	return nil
}

//全覆盖更新
func Update(obj interface{}) error {
	val := reflect.ValueOf(obj)
	typ := reflect.Indirect(val).Type()
	tableName := typ.Name()

	db.RLock()
	if _, ok := db.tables[tableName]; !ok {
		panic(fmt.Errorf("table %s is not exsit", tableName))
	}
	db.RUnlock()

	id := obj.(Row).GetID()

	lock := db.locks[tableName]
	lock.Lock()
	defer lock.Unlock()

	if rid, ok := db.rows[tableName][id]; ok {
		db.tables[tableName][rid] = obj
		//更新versions
		ver := db.versions[tableName][id]
		ver.Version += 1
		ver.UpdateStamp = time.Now().Unix()

		//发起持久化指令
		putTrx(&Transaction{Cmd: "UPDATE", TableName: tableName, ID: id, Version: (db.versions[tableName][id]).Version})

		//log.Printf("update record id[%d] in table %s's %d row", id, tableName, rid)

	} else {
		log.Printf("record %d is not exist in table %s", id, tableName)
		return fmt.Errorf("record %d is not exist in table %s", id, tableName)
	}
	return nil
}

//回调更新，用户转账、打工等场景  todo
func UpdateFunc() error {
	return nil
}

func Get(obj interface{}) interface{} {
	val := reflect.ValueOf(obj)
	typ := reflect.Indirect(val).Type()
	tableName := typ.Name()

	db.RLock()
	if _, ok := db.tables[tableName]; !ok {
		panic(fmt.Errorf("table %s is not exsit", tableName))
	}
	db.RUnlock()

	id := obj.(Row).GetID()
	lock := db.locks[tableName]
	lock.Lock()
	defer lock.Unlock()

	if rid, ok := db.rows[tableName][id]; ok {
		return db.tables[tableName][rid]
	}

	log.Printf("record %d is not exist in table %s", id, tableName)
	return nil
}

func Delete(obj interface{}) {
	val := reflect.ValueOf(obj)
	typ := reflect.Indirect(val).Type()
	tableName := typ.Name()

	db.RLock()
	if _, ok := db.tables[tableName]; !ok {
		panic(fmt.Errorf("table %s is not exsit", tableName))
	}
	db.RUnlock()

	id := obj.(Row).GetID()
	lock := db.locks[tableName]
	lock.Lock()
	defer lock.Unlock()

	rid, ok := db.rows[tableName][id]
	if !ok {
		return
	}

	ver := db.versions[tableName][id]
	delete(db.rows[tableName], id)
	delete(db.versions[tableName], id)
	put(tableName, rid)

	//发起持久化指令
	putTrx(&Transaction{Cmd: "DELETE", TableName: tableName, ID: id, Version: ver.Version})

	//删除主键索引
	pk := PRIMARYKEY
	for k := 0; k < len(db.indexs[tableName][pk]); k++ {
		if db.indexs[tableName][pk][k] == id {
			db.indexs[tableName][pk][k] = db.indexs[tableName][pk][len(db.indexs[tableName][pk])-1]
			db.indexs[tableName][pk] = db.indexs[tableName][pk][:len(db.indexs[tableName][pk])-1]
		}
	}
	//索引排序
	sortIndex(tableName, pk)

	log.Printf("delete recoed %d from %s", id, tableName)

	indexs := obj.(Row).Index()
	if indexs == nil {
		return
	}

	//存在索引，删除索引
	for i := 0; i < len(indexs); i++ {
		if len(indexs[i]) == 0 {
			continue
		}
		pk := tableName
		sort.StringSlice(indexs[i]).Sort()
		for j := 0; j < len(indexs[i]); j++ {
			pk += fmt.Sprintf(":%s:%v", indexs[i][j], reflect.Indirect(val).FieldByName(indexs[i][j]))
		}
		for k := 0; k < len(db.indexs[tableName][pk]); k++ {
			if db.indexs[tableName][pk][k] == id {
				db.indexs[tableName][pk][k] = db.indexs[tableName][pk][len(db.indexs[tableName][pk])-1]
				db.indexs[tableName][pk] = db.indexs[tableName][pk][:len(db.indexs[tableName][pk])-1]
			}
		}
		//索引排序
		sortIndex(tableName, pk)
	}
	//log.Printf("index is %+v", db.indexs[tableName])
}
