package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dgrijalva/jwt-go"
	"github.com/go-sql-driver/mysql"
	"github.com/gorilla/sessions"
	"github.com/jmoiron/sqlx"
	"github.com/kaz/pprotein/integration/standalone"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/labstack/gommon/log"
	"github.com/motoki317/sc"
)

const (
	sessionName                 = "isucondition_go"
	conditionLimit              = 20
	frontendContentsPath        = "../public"
	jiaJWTSigningKeyPath        = "../ec256-public.pem"
	defaultIconFilePath         = "../NoImage.jpg"
	isuImagesPath               = "../isu-images"
	defaultJIAServiceURL        = "http://localhost:5000"
	mysqlErrNumDuplicateEntry   = 1062
	conditionLevelInfo          = "info"
	conditionLevelWarning       = "warning"
	conditionLevelCritical      = "critical"
	scoreConditionLevelInfo     = 3
	scoreConditionLevelWarning  = 2
	scoreConditionLevelCritical = 1
)

var (
	db                  *sqlx.DB
	sessionStore        sessions.Store
	mySQLConnectionData *MySQLConnectionEnv

	jiaJWTSigningKey *ecdsa.PublicKey

	postIsuConditionTargetBaseURL string // JIAへのactivate時に登録する，ISUがconditionを送る先のURL
)

type Config struct {
	Name string `db:"name"`
	URL  string `db:"url"`
}

type Isu struct {
	ID         int       `db:"id" json:"id"`
	JIAIsuUUID string    `db:"jia_isu_uuid" json:"jia_isu_uuid"`
	Name       string    `db:"name" json:"name"`
	Character  string    `db:"character" json:"character"`
	JIAUserID  string    `db:"jia_user_id" json:"-"`
	CreatedAt  time.Time `db:"created_at" json:"-"`
	UpdatedAt  time.Time `db:"updated_at" json:"-"`
}

type IsuFromJIA struct {
	Character string `json:"character"`
}

type GetIsuListResponse struct {
	ID                 int                      `json:"id"`
	JIAIsuUUID         string                   `json:"jia_isu_uuid"`
	Name               string                   `json:"name"`
	Character          string                   `json:"character"`
	LatestIsuCondition *GetIsuConditionResponse `json:"latest_isu_condition"`
}

type IsuCondition struct {
	JIAIsuUUID     string    `db:"jia_isu_uuid"`
	Timestamp      time.Time `db:"timestamp"`
	IsSitting      bool      `db:"is_sitting"`
	Condition      string    `db:"condition"`
	Message        string    `db:"message"`
	CreatedAt      time.Time `db:"created_at"`
	ConditionLevel string    `db:"condition_level"`
}

type GetIsuCondition struct {
	JIAIsuUUID     string    `db:"jia_isu_uuid"`
	Timestamp      time.Time `db:"timestamp"`
	IsSitting      bool      `db:"is_sitting"`
	Condition      string    `db:"condition"`
	Message        string    `db:"message"`
	CreatedAt      time.Time `db:"created_at"`
	ConditionLevel string    `db:"condition_level"`
}

type MySQLConnectionEnv struct {
	Host     string
	Port     string
	User     string
	DBName   string
	Password string
}

type InitializeRequest struct {
	JIAServiceURL string `json:"jia_service_url"`
}

type InitializeResponse struct {
	Language string `json:"language"`
}

type GetMeResponse struct {
	JIAUserID string `json:"jia_user_id"`
}

type GraphResponse struct {
	StartAt             int64           `json:"start_at"`
	EndAt               int64           `json:"end_at"`
	Data                *GraphDataPoint `json:"data"`
	ConditionTimestamps []int64         `json:"condition_timestamps"`
}

type GraphDataPoint struct {
	Score      int                  `json:"score"`
	Percentage ConditionsPercentage `json:"percentage"`
}

type ConditionsPercentage struct {
	Sitting      int `json:"sitting"`
	IsBroken     int `json:"is_broken"`
	IsDirty      int `json:"is_dirty"`
	IsOverweight int `json:"is_overweight"`
}

type GraphDataPointWithInfo struct {
	JIAIsuUUID          string
	StartAt             time.Time
	Data                GraphDataPoint
	ConditionTimestamps []int64
}

type GetIsuConditionResponse struct {
	JIAIsuUUID     string `json:"jia_isu_uuid"`
	IsuName        string `json:"isu_name"`
	Timestamp      int64  `json:"timestamp"`
	IsSitting      bool   `json:"is_sitting"`
	Condition      string `json:"condition"`
	ConditionLevel string `json:"condition_level"`
	Message        string `json:"message"`
}

type TrendResponse struct {
	Character string            `json:"character"`
	Info      []*TrendCondition `json:"info"`
	Warning   []*TrendCondition `json:"warning"`
	Critical  []*TrendCondition `json:"critical"`
}

type TrendCondition struct {
	ID        int   `json:"isu_id"`
	Timestamp int64 `json:"timestamp"`
}

type PostIsuConditionRequest struct {
	IsSitting bool   `json:"is_sitting"`
	Condition string `json:"condition"`
	Message   string `json:"message"`
	Timestamp int64  `json:"timestamp"`
}

type JIAServiceRequest struct {
	TargetBaseURL string `json:"target_base_url"`
	IsuUUID       string `json:"isu_uuid"`
}

func getEnv(key string, defaultValue string) string {
	val := os.Getenv(key)
	if val != "" {
		return val
	}
	return defaultValue
}

func NewMySQLConnectionEnv() *MySQLConnectionEnv {
	return &MySQLConnectionEnv{
		Host:     getEnv("MYSQL_HOST", "127.0.0.1"),
		Port:     getEnv("MYSQL_PORT", "3306"),
		User:     getEnv("MYSQL_USER", "isucon"),
		DBName:   getEnv("MYSQL_DBNAME", "isucondition"),
		Password: getEnv("MYSQL_PASS", "isucon"),
	}
}

func (mc *MySQLConnectionEnv) ConnectDB() (*sqlx.DB, error) {
	dsn := fmt.Sprintf("%v:%v@tcp(%v:%v)/%v?parseTime=true&loc=Asia%%2FTokyo&interpolateParams=true", mc.User, mc.Password, mc.Host, mc.Port, mc.DBName)
	return sqlx.Open("mysql", dsn)
}

func init() {
	sessionStore = sessions.NewCookieStore([]byte(getEnv("SESSION_KEY", "isucondition")))

	key, err := ioutil.ReadFile(jiaJWTSigningKeyPath)
	if err != nil {
		log.Fatalf("failed to read file: %v", err)
	}
	jiaJWTSigningKey, err = jwt.ParseECPublicKeyFromPEM(key)
	if err != nil {
		log.Fatalf("failed to parse ECDSA public key: %v", err)
	}

	http.DefaultTransport.(*http.Transport).MaxIdleConns = 0                // infinite
	http.DefaultTransport.(*http.Transport).MaxIdleConnsPerHost = 1024 * 16 // default: 2
	http.DefaultTransport.(*http.Transport).ForceAttemptHTTP2 = true        // go1.13以上
}

var doPostLock = sync.Mutex{}

func doPostIsuCondition() {
	if len(postIsuConditionRequests) == 0 {
		return
	}
	//fmt.Println("doPostLock Locked")
	doPostLock.Lock()
	doRequest := make([]PostIsuConditionRequests, len(postIsuConditionRequests))
	copy(doRequest, postIsuConditionRequests)
	postIsuConditionRequests = []PostIsuConditionRequests{}
	args := make([]BulkInsertArg, 0, len(doRequest))

	insertedIsuUUIDMap := map[string]bool{}
	for _, cond := range doRequest {
		insertedIsuUUIDMap[cond.JiaIsuUUID] = true
	}
	insertedIsuUUID := make([]string, 0, len(insertedIsuUUIDMap))
	for key, _ := range insertedIsuUUIDMap {
		insertedIsuUUID = append(insertedIsuUUID, key)
	}

	for _, cond := range doRequest {
		timestamp := time.Unix(cond.Timestamp, 0)
		args = append(args, BulkInsertArg{
			JiaIsuUUID:     cond.JiaIsuUUID,
			Timestamp:      timestamp,
			IsSitting:      cond.IsSitting,
			Condition:      cond.Condition,
			Message:        cond.Message,
			ConditionLevel: cond.ConditionLevel,
		})

		isuConditionCacheByIsuUUID.Forget(cond.JiaIsuUUID)
	}

	//fmt.Println("INSERT POST ISU CONDITION")
	_, err := db.NamedExec("INSERT INTO `isu_condition` (`jia_isu_uuid`, `timestamp`, `is_sitting`, `condition`, `message`, `condition_level`) VALUES (:jia_isu_uuid, :timestamp, :is_sitting, :condition, :message, :condition_level)", args)
	if err != nil {
		fmt.Printf("db error post isu condition: %v", err)
	}
	fmt.Printf("PostIsuCondition Inserted %v\n", len(doRequest))
	doPostLock.Unlock()

	go func() {
		err := postIsuConditionCache(insertedIsuUUID)
		if err != nil {
			fmt.Println(err)
			return
		}
	}()

	//fmt.Println("doPostLock UnLocked")
	// query := "INSERT INTO `isu_condition` (`jia_isu_uuid`, `timestamp`, `is_sitting`, `condition`, `message`, `condition_level`) VALUES "
	// for i, cond := range doRequest {
	// 	timestamp := time.Unix(cond.Timestamp, 0)

	// 	if i > 0 {
	// 		query += ", "
	// 	}
	// 	query += "(?, ?, ?, ?, ?, ?)"
	// 	args = append(args, cond.JiaIsuUUID, timestamp, cond.IsSitting, cond.Condition, cond.Message, cond.ConditionLevel)
	// 	isuConditionCacheByIsuUUID.Forget(cond.JiaIsuUUID)
	// }
	// default: tx
	// if _, err = db.Exec(query, args...); err != nil {
	// 	// fmt.Println("POST DB error: %v", err)
	// }
	//err = tx.Commit()
	//if err != nil {
	//	c.Logger().Errorf("db error: %v", err)
	//	return c.NoContent(http.StatusInternalServerError)
	//}
}

type PostIsuConditionCacheRequest struct {
	UUIDs []string `json:"uuids"`
}

func postIsuConditionCache(insertedIsuUUID []string) error {
	data := PostIsuConditionCacheRequest{UUIDs: insertedIsuUUID}

	jsonData, err := json.Marshal(data)
	if err != nil {
		return err
	}

	_, err = http.Post("http://172.31.38.16/cache/isuConditionCacheByIsuUUID", "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		return err
	}
	return nil
}
func forgetIsuConditionCacheByIsuUUID(c echo.Context) error {
	p := new(PostIsuConditionCacheRequest)
	if err := c.Bind(p); err != nil {
		return c.NoContent(http.StatusBadRequest)
	}

	fmt.Printf("Forget %v\n", len(p.UUIDs))
	for _, uuid := range p.UUIDs {
		isuConditionCacheByIsuUUID.Forget(uuid)
	}

	return c.NoContent(http.StatusOK)
}

func updateTrend() {
	var isuList []Isu
	isuList = isuCache.GetAll()
	if len(isuList) == 0 || isuList == nil {
		err := db.Select(&isuList, "SELECT * FROM `isu`")
		if errors.Is(err, sql.ErrNoRows) {
			fmt.Println("no rows tickerGetTrend")
			return
		}
		if err != nil {
			fmt.Printf("db error: %v", err)
			return
		}
		isuCache.Set(isuList)
	}
	characterIsuMap := map[string][]Isu{}
	for _, isu := range isuList {
		characterIsuMap[isu.Character] = append(characterIsuMap[isu.Character], isu)
	}

	var res []TrendResponse

	for character, isuLists := range characterIsuMap {
		var characterInfoIsuConditions []*TrendCondition
		var characterWarningIsuConditions []*TrendCondition
		var characterCriticalIsuConditions []*TrendCondition
		for _, isu := range isuLists {
			//isuLastCondition, err := isuConditionCacheByIsuUUID.Get(context.Background(), isu.JIAIsuUUID)
			var isuLastCondition IsuCondition
			lastConditionPointer, err := isuConditionCacheByIsuUUID.Get(context.Background(), isu.JIAIsuUUID)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					continue
				} else {
					fmt.Printf("db error get trend: %v", err)
					return
				}
			}
			if lastConditionPointer == nil {
				continue
			}
			isuLastCondition = *lastConditionPointer

			//err := db.Get(&isuLastCondition, "SELECT * FROM `isu_condition` WHERE `jia_isu_uuid` = ? ORDER BY `timestamp` DESC LIMIT 1", isu.JIAIsuUUID)
			//if errors.Is(err, sql.ErrNoRows) {
			//	continue
			//}
			//if err != nil {
			//	fmt.Printf("db error: %v", err)
			//	return
			//}

			conditionLevel, err := calculateConditionLevel(isuLastCondition.Condition)
			trendCondition := TrendCondition{
				ID:        isu.ID,
				Timestamp: isuLastCondition.Timestamp.Unix(),
			}
			switch conditionLevel {
			case "info":
				characterInfoIsuConditions = append(characterInfoIsuConditions, &trendCondition)
			case "warning":
				characterWarningIsuConditions = append(characterWarningIsuConditions, &trendCondition)
			case "critical":
				characterCriticalIsuConditions = append(characterCriticalIsuConditions, &trendCondition)
			}

		}

		sort.Slice(characterInfoIsuConditions, func(i, j int) bool {
			return characterInfoIsuConditions[i].Timestamp > characterInfoIsuConditions[j].Timestamp
		})
		sort.Slice(characterWarningIsuConditions, func(i, j int) bool {
			return characterWarningIsuConditions[i].Timestamp > characterWarningIsuConditions[j].Timestamp
		})
		sort.Slice(characterCriticalIsuConditions, func(i, j int) bool {
			return characterCriticalIsuConditions[i].Timestamp > characterCriticalIsuConditions[j].Timestamp
		})
		res = append(res,
			TrendResponse{
				Character: character,
				Info:      characterInfoIsuConditions,
				Warning:   characterWarningIsuConditions,
				Critical:  characterCriticalIsuConditions,
			})
	}

	trendResponse = res
}

func main() {
	go standalone.Integrate(":8888")

	e := echo.New()
	// e.Debug = true
	// e.Logger.SetLevel(log.DEBUG)

	// e.Use(middleware.Logger())
	e.Use(middleware.Recover())

	e.POST("/initialize", postInitialize)

	e.POST("/api/auth", postAuthentication)
	e.POST("/api/signout", postSignout)
	e.GET("/api/user/me", getMe)
	e.GET("/api/isu", getIsuList)
	e.POST("/api/isu", postIsu)
	e.GET("/api/isu/:jia_isu_uuid", getIsuID)
	e.GET("/api/isu/:jia_isu_uuid/icon", getIsuIcon)
	e.GET("/api/isu/:jia_isu_uuid/graph", getIsuGraph)
	e.GET("/api/condition/:jia_isu_uuid", getIsuConditions)
	e.GET("/api/trend", getTrend)

	e.POST("/api/condition/:jia_isu_uuid", postIsuCondition)

	e.GET("/", getIndex)
	e.GET("/isu/:jia_isu_uuid", getIndex)
	e.GET("/isu/:jia_isu_uuid/condition", getIndex)
	e.GET("/isu/:jia_isu_uuid/graph", getIndex)
	e.GET("/register", getIndex)
	e.Static("/assets", frontendContentsPath+"/assets")

	e.POST("/cache/isuConditionCacheByIsuUUID", forgetIsuConditionCacheByIsuUUID)

	mySQLConnectionData = NewMySQLConnectionEnv()

	var err error
	db, err = mySQLConnectionData.ConnectDB()
	if err != nil {
		e.Logger.Fatalf("failed to connect db: %v", err)
		return
	}
	db.SetMaxOpenConns(1024 * 2)
	db.SetMaxIdleConns(1024 * 2)
	defer db.Close()

	postIsuConditionTargetBaseURL = os.Getenv("POST_ISUCONDITION_TARGET_BASE_URL")
	if postIsuConditionTargetBaseURL == "" {
		e.Logger.Fatalf("missing: POST_ISUCONDITION_TARGET_BASE_URL")
		return
	}

	isuConditionCacheByIsuUUID, err = sc.New[string, *IsuCondition](getIsuConditionsByIsuUUID, time.Hour, time.Hour)
	if err != nil {
		e.Logger.Fatalf("failed to create cache: %v", err)
		return
	}
	isuCountByIsuUUID, err = sc.New[string, *int](getIsuCountByIsuUUID, time.Hour, time.Hour)
	if err != nil {
		e.Logger.Fatalf("failed to create cache: %v", err)
		return
	}
	//cacheIsu, err = sc.New[string, *Isu](cacheIsuGet, time.Minute, time.Minute)
	//if err != nil {
	//	e.Logger.Fatalf("failed to create cache: %v", err)
	//	return
	//}

	isuCache = IsuCache{Isu: make(map[string]Isu)}

	tickerPostIsuConditionEnv := os.Getenv("TICKER_POST_ISU_CONDITION")
	if tickerPostIsuConditionEnv != "" {
		postIsuConditionTime, err := strconv.Atoi(tickerPostIsuConditionEnv)
		if err != nil {
			e.Logger.Fatalf("failed to convert TICKER_POST_ISU_CONDITION: %v", err)
		} else {
			tickerPostIsuCondition := time.NewTicker(time.Duration(postIsuConditionTime) * time.Millisecond)
			go func() {
				for {
					select {
					case _ = <-tickerPostIsuCondition.C:
						doPostIsuCondition()
					}
				}
			}()
		}
	}

	tickerGetTrendEnv := os.Getenv("TICKER_GET_TREND")
	if tickerGetTrendEnv != "" {
		getTrendTime, err := strconv.Atoi(tickerGetTrendEnv)
		if err != nil {
			e.Logger.Fatalf("failed to convert TICKER_GET_TREND: %v", err)
		} else {
			tickerGetTrend := time.NewTicker(time.Duration(getTrendTime) * time.Millisecond)
			go func() {
				for {
					select {
					case _ = <-tickerGetTrend.C:
						updateTrend()
					}
				}
			}()
		}
	}

	serverPort := fmt.Sprintf(":%v", getEnv("SERVER_APP_PORT", "3000"))
	e.Logger.Fatal(e.Start(serverPort))
}

type BulkInsertArg struct {
	JiaIsuUUID     string    `db:"jia_isu_uuid"`
	Timestamp      time.Time `db:"timestamp"`
	IsSitting      bool      `db:"is_sitting"`
	Condition      string    `db:"condition"`
	Message        string    `db:"message"`
	ConditionLevel string    `db:"condition_level"`
}

func getSession(r *http.Request) (*sessions.Session, error) {
	session, err := sessionStore.Get(r, sessionName)
	if err != nil {
		return nil, err
	}
	return session, nil
}

func getUserIDFromSession(c echo.Context) (string, int, error) {
	session, err := getSession(c.Request())
	if err != nil {
		return "", http.StatusInternalServerError, fmt.Errorf("failed to get session: %v", err)
	}
	_jiaUserID, ok := session.Values["jia_user_id"]
	if !ok {
		return "", http.StatusUnauthorized, fmt.Errorf("no session")
	}

	jiaUserID := _jiaUserID.(string)
	//var count int
	//
	//err = db.Get(&count, "SELECT COUNT(*) FROM `user` WHERE `jia_user_id` = ?",
	//	jiaUserID)
	//if err != nil {
	//	return "", http.StatusInternalServerError, fmt.Errorf("db error: %v", err)
	//}
	//
	//if count == 0 {
	//	return "", http.StatusUnauthorized, fmt.Errorf("not found: user")
	//}

	return jiaUserID, 0, nil
}

func getJIAServiceURL(tx *sqlx.Tx) string {
	var config Config
	err := tx.Get(&config, "SELECT * FROM `isu_association_config` WHERE `name` = ?", "jia_service_url")
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.Print(err)
		}
		return defaultJIAServiceURL
	}
	return config.URL
}

var isuConditionCacheByIsuUUID *sc.Cache[string, *IsuCondition]

func getIsuConditionsByIsuUUID(_ context.Context, isuUUID string) (*IsuCondition, error) {
	var condition IsuCondition
	err := db.Get(&condition, "SELECT * FROM `isu_condition` WHERE `jia_isu_uuid` = ? ORDER BY `timestamp` DESC LIMIT 1", isuUUID)
	if err != nil {
		return nil, err
	}
	return &condition, nil
}

var isuCountByIsuUUID *sc.Cache[string, *int]
var notFound = errors.New("not found")

func getIsuCountByIsuUUID(_ context.Context, isuUUID string) (*int, error) {
	var count int
	err := db.Get(&count, "SELECT COUNT(*) FROM `isu` WHERE `jia_isu_uuid` = ?", isuUUID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, notFound
	}
	return &count, nil
}

var cacheIsu = sc.NewMust[string, *Isu](cacheIsuGet, 300*time.Hour, 300*time.Hour)

func cacheIsuGet(_ context.Context, isuUUID string) (*Isu, error) {
	var isu Isu
	err := db.Get(&isu, "SELECT * FROM `isu` WHERE `jia_isu_uuid` = ?", isuUUID)
	if err != nil {
		return nil, err
	}
	return &isu, nil
}

var cacheGetIsuList = sc.NewMust[string, *[]GetIsuListResponse](cacheGetIsuListGet, 300*time.Hour, 300*time.Hour)
var statusInternalServerError = errors.New("internal server error")

func cacheGetIsuListGet(_ context.Context, userUUID string) (*[]GetIsuListResponse, error) {
	tx, err := db.Beginx()
	if err != nil {
		return nil, statusInternalServerError
	}
	defer tx.Rollback()

	isuList := []Isu{}
	err = tx.Select(
		&isuList,
		"SELECT * FROM `isu` WHERE `jia_user_id` = ? ORDER BY `id` DESC",
		userUUID)
	if err != nil {
		return nil, statusInternalServerError
	}

	responseList := []GetIsuListResponse{}
	for _, isu := range isuList {
		var lastCondition IsuCondition
		foundLastCondition := true
		lastConditionPointer, err := isuConditionCacheByIsuUUID.Get(context.Background(), isu.JIAIsuUUID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				foundLastCondition = false
			} else {
				return nil, statusInternalServerError
			}
		}
		if lastConditionPointer == nil {
			foundLastCondition = false
		} else {
			lastCondition = *lastConditionPointer
		}

		//err = tx.Get(&lastCondition, "SELECT * FROM `isu_condition` WHERE `jia_isu_uuid` = ? ORDER BY `timestamp` DESC LIMIT 1",
		//	isu.JIAIsuUUID)
		//if err != nil {
		//	if errors.Is(err, sql.ErrNoRows) {
		//		foundLastCondition = false
		//	} else {
		//		c.Logger().Errorf("db error: %v", err)
		//		return c.NoContent(http.StatusInternalServerError)
		//	}
		//}

		var formattedCondition *GetIsuConditionResponse
		if foundLastCondition {
			conditionLevel, err := calculateConditionLevel(lastCondition.Condition)
			if err != nil {
				return nil, statusInternalServerError
			}

			formattedCondition = &GetIsuConditionResponse{
				JIAIsuUUID:     lastCondition.JIAIsuUUID,
				IsuName:        isu.Name,
				Timestamp:      lastCondition.Timestamp.Unix(),
				IsSitting:      lastCondition.IsSitting,
				Condition:      lastCondition.Condition,
				ConditionLevel: conditionLevel,
				Message:        lastCondition.Message,
			}
		}

		res := GetIsuListResponse{
			ID:                 isu.ID,
			JIAIsuUUID:         isu.JIAIsuUUID,
			Name:               isu.Name,
			Character:          isu.Character,
			LatestIsuCondition: formattedCondition}
		responseList = append(responseList, res)
	}

	err = tx.Commit()
	if err != nil {
		return nil, statusInternalServerError
	}

	return &responseList, nil
}

type IsuCache struct {
	Isu map[string]Isu
	mu  sync.Mutex
}

var isuCache IsuCache

func (c *IsuCache) Set(isu []Isu) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, i := range isu {
		c.Isu[i.JIAIsuUUID] = i
	}
}

func (c *IsuCache) GetAll() []Isu {
	c.mu.Lock()
	defer c.mu.Unlock()
	isu := make([]Isu, 0, len(c.Isu))
	for _, i := range c.Isu {
		isu = append(isu, i)
	}
	return isu
}

// POST /initialize
// サービスを初期化
func postInitialize(c echo.Context) error {
	if os.Getenv("SERVER_ID") == "3" {
		fmt.Println("Cache Purged")
		isuConditionCacheByIsuUUID.Purge()
		isuCountByIsuUUID.Purge()
		cacheIsu.Purge()

		return c.JSON(http.StatusOK, InitializeResponse{
			Language: "go",
		})
	}

	go func() {
		if _, err := http.Get("http://p.isucon.ikura-hamu.work/api/group/collect"); err != nil {
			log.Printf("failed to communicate with pprotein: %v", err)
		}
	}()

	var request InitializeRequest
	err := c.Bind(&request)
	if err != nil {
		return c.String(http.StatusBadRequest, "bad request body")
	}

	reciver_err := make(chan error)
	go func() {
		fmt.Println("Initialize Requested to s3")
		defer close(reciver_err)
		req, err := http.NewRequest(http.MethodPost, "http://172.31.38.18/initialize", bytes.NewBuffer([]byte{}))
		if err != nil {
			reciver_err <- err
			return
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			reciver_err <- err
			return
		}
		defer res.Body.Close()

		if res.StatusCode != http.StatusOK {
			reciver_err <- fmt.Errorf("Initialize returned error: status code %v", res.StatusCode)
			return
		}
	}()

	cmd := exec.Command("../sql/init.sh")
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stderr
	err = cmd.Run()
	if err != nil {
		c.Logger().Errorf("exec init.sh error: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}

	_, err = db.Exec(
		"INSERT INTO `isu_association_config` (`name`, `url`) VALUES (?, ?) ON DUPLICATE KEY UPDATE `url` = VALUES(`url`)",
		"jia_service_url",
		request.JIAServiceURL,
	)
	if err != nil {
		c.Logger().Errorf("db error : %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}

	isuConditionCacheByIsuUUID.Purge()
	isuCountByIsuUUID.Purge()
	cacheIsu.Purge()

	_, err = db.Exec("ALTER TABLE `isu_condition` ADD COLUMN `condition_level` VARCHAR(255) DEFAULT ''")
	if err != nil {
		fmt.Println(err)
	}
	_, err = db.Exec("ALTER TABLE `isu_condition` DROP COLUMN `id`;")
	if err != nil {
		fmt.Println(err)
	}
	_, err = db.Exec("ALTER TABLE isu DROP COLUMN image;")
	if err != nil {
		fmt.Println(err)
	}

	var conditions []IsuCondition
	err = db.Select(&conditions, "SELECT * FROM `isu_condition`")
	if err != nil {
		fmt.Println(err)
	}

	for _, condition := range conditions {
		conditionLevel, err := calculateConditionLevel(condition.Condition)
		if err != nil {
			fmt.Println(err)
		}

		_, err = db.Exec("UPDATE `isu_condition` SET `condition_level` = ? WHERE `jia_isu_uuid` = ? AND `timestamp` = ?", conditionLevel, condition.JIAIsuUUID, condition.Timestamp)
		if err != nil {
			fmt.Println(err)
		}
	}

	return c.JSON(http.StatusOK, InitializeResponse{
		Language: "go",
	})
}

// POST /api/auth
// サインアップ・サインイン
func postAuthentication(c echo.Context) error {
	reqJwt := strings.TrimPrefix(c.Request().Header.Get("Authorization"), "Bearer ")

	token, err := jwt.Parse(reqJwt, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodECDSA); !ok {
			return nil, jwt.NewValidationError(fmt.Sprintf("unexpected signing method: %v", token.Header["alg"]), jwt.ValidationErrorSignatureInvalid)
		}
		return jiaJWTSigningKey, nil
	})
	if err != nil {
		switch err.(type) {
		case *jwt.ValidationError:
			return c.String(http.StatusForbidden, "forbidden")
		default:
			c.Logger().Error(err)
			return c.NoContent(http.StatusInternalServerError)
		}
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		c.Logger().Errorf("invalid JWT payload")
		return c.NoContent(http.StatusInternalServerError)
	}
	jiaUserIDVar, ok := claims["jia_user_id"]
	if !ok {
		return c.String(http.StatusBadRequest, "invalid JWT payload")
	}
	jiaUserID, ok := jiaUserIDVar.(string)
	if !ok {
		return c.String(http.StatusBadRequest, "invalid JWT payload")
	}

	_, err = db.Exec("INSERT IGNORE INTO user (`jia_user_id`) VALUES (?)", jiaUserID)
	if err != nil {
		c.Logger().Errorf("db error: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}

	session, err := getSession(c.Request())
	if err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	session.Values["jia_user_id"] = jiaUserID
	err = session.Save(c.Request(), c.Response())
	if err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	return c.NoContent(http.StatusOK)
}

// POST /api/signout
// サインアウト
func postSignout(c echo.Context) error {
	_, errStatusCode, err := getUserIDFromSession(c)
	if err != nil {
		if errStatusCode == http.StatusUnauthorized {
			return c.String(http.StatusUnauthorized, "you are not signed in")
		}

		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	session, err := getSession(c.Request())
	if err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	session.Options = &sessions.Options{MaxAge: -1, Path: "/"}
	err = session.Save(c.Request(), c.Response())
	if err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	return c.NoContent(http.StatusOK)
}

// GET /api/user/me
// サインインしている自分自身の情報を取得
func getMe(c echo.Context) error {
	jiaUserID, errStatusCode, err := getUserIDFromSession(c)
	if err != nil {
		if errStatusCode == http.StatusUnauthorized {
			return c.String(http.StatusUnauthorized, "you are not signed in")
		}

		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	res := GetMeResponse{JIAUserID: jiaUserID}
	return c.JSON(http.StatusOK, res)
}

// GET /api/isu
// ISUの一覧を取得
func getIsuList(c echo.Context) error {
	jiaUserID, errStatusCode, err := getUserIDFromSession(c)
	if err != nil {
		if errStatusCode == http.StatusUnauthorized {
			return c.String(http.StatusUnauthorized, "you are not signed in")
		}

		c.Logger().Error(err)
		fmt.Println("bad40")
		return c.NoContent(http.StatusInternalServerError)
	}

	//responseListPointer, err := cacheGetIsuList.Get(context.Background(), jiaUserID)

	tx, err := db.Beginx()
	if err != nil {
		return c.NoContent(http.StatusInternalServerError)
	}
	defer tx.Rollback()

	var isuList []Isu
	err = tx.Select(
		&isuList,
		"SELECT * FROM `isu` WHERE `jia_user_id` = ? ORDER BY `id` DESC",
		jiaUserID)
	if err != nil {
		return c.NoContent(http.StatusInternalServerError)
	}

	responseList := []GetIsuListResponse{}
	for _, isu := range isuList {
		var lastCondition IsuCondition
		foundLastCondition := true
		lastConditionPointer, err := isuConditionCacheByIsuUUID.Get(context.Background(), isu.JIAIsuUUID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				foundLastCondition = false
			} else {
				return c.NoContent(http.StatusInternalServerError)
			}
		}
		if lastConditionPointer == nil {
			foundLastCondition = false
		} else {
			lastCondition = *lastConditionPointer
		}

		//err = tx.Get(&lastCondition, "SELECT * FROM `isu_condition` WHERE `jia_isu_uuid` = ? ORDER BY `timestamp` DESC LIMIT 1",
		//	isu.JIAIsuUUID)
		//if err != nil {
		//	if errors.Is(err, sql.ErrNoRows) {
		//		foundLastCondition = false
		//	} else {
		//		c.Logger().Errorf("db error: %v", err)
		//		return c.NoContent(http.StatusInternalServerError)
		//	}
		//}

		var formattedCondition *GetIsuConditionResponse
		if foundLastCondition {
			conditionLevel, err := calculateConditionLevel(lastCondition.Condition)
			if err != nil {
				return c.NoContent(http.StatusInternalServerError)
			}

			formattedCondition = &GetIsuConditionResponse{
				JIAIsuUUID:     lastCondition.JIAIsuUUID,
				IsuName:        isu.Name,
				Timestamp:      lastCondition.Timestamp.Unix(),
				IsSitting:      lastCondition.IsSitting,
				Condition:      lastCondition.Condition,
				ConditionLevel: conditionLevel,
				Message:        lastCondition.Message,
			}
		}

		res := GetIsuListResponse{
			ID:                 isu.ID,
			JIAIsuUUID:         isu.JIAIsuUUID,
			Name:               isu.Name,
			Character:          isu.Character,
			LatestIsuCondition: formattedCondition}
		responseList = append(responseList, res)
	}

	err = tx.Commit()
	if err != nil {
		return c.NoContent(http.StatusInternalServerError)
	}

	return c.JSON(http.StatusOK, responseList)
}

// POST /api/isu
// ISUを登録
func postIsu(c echo.Context) error {
	jiaUserID, errStatusCode, err := getUserIDFromSession(c)
	if err != nil {
		if errStatusCode == http.StatusUnauthorized {
			return c.String(http.StatusUnauthorized, "you are not signed in")
		}

		c.Logger().Error(err)
		fmt.Println("bad50")
		return c.NoContent(http.StatusInternalServerError)
	}

	useDefaultImage := false

	jiaIsuUUID := c.FormValue("jia_isu_uuid")
	//fmt.Printf("PostIsu: %v\n", jiaIsuUUID)
	isuName := c.FormValue("isu_name")
	fh, err := c.FormFile("image")
	if err != nil {
		if !errors.Is(err, http.ErrMissingFile) {
			fmt.Println("bad51")
			return c.String(http.StatusBadRequest, "bad format: icon")
		}
		useDefaultImage = true
	}

	var image []byte

	if useDefaultImage {
		image, err = ioutil.ReadFile(defaultIconFilePath)
		if err != nil {
			c.Logger().Error(err)
			fmt.Println("bad52")
			return c.NoContent(http.StatusInternalServerError)
		}
	} else {
		file, err := fh.Open()
		if err != nil {
			c.Logger().Error(err)
			fmt.Println("bad53")
			return c.NoContent(http.StatusInternalServerError)
		}
		defer file.Close()

		image, err = ioutil.ReadAll(file)
		if err != nil {
			c.Logger().Error(err)
			fmt.Println("bad54")
			return c.NoContent(http.StatusInternalServerError)
		}
	}

	err = os.WriteFile(filepath.Join(isuImagesPath, jiaIsuUUID), image, 0644)
	if err != nil {
		c.Logger().Errorf("write image file error: %v", err)
		fmt.Println("bad55")
		return c.NoContent(http.StatusInternalServerError)
	}

	tx, err := db.Beginx()
	if err != nil {
		c.Logger().Errorf("db error: %v", err)
		fmt.Println("bad56")
		return c.NoContent(http.StatusInternalServerError)
	}
	defer tx.Rollback()
	_, err = tx.Exec("INSERT INTO `isu` (`jia_isu_uuid`, `name`, `jia_user_id`) VALUES (?, ?, ?)",
		jiaIsuUUID, isuName, jiaUserID)
	if err != nil {
		mysqlErr, ok := err.(*mysql.MySQLError)

		if ok && mysqlErr.Number == uint16(mysqlErrNumDuplicateEntry) {
			return c.String(http.StatusConflict, "duplicated: isu")
		}

		c.Logger().Errorf("db error: %v", err)
		fmt.Println("bad57")
		return c.NoContent(http.StatusInternalServerError)
	}

	targetURL := getJIAServiceURL(tx) + "/api/activate"
	body := JIAServiceRequest{postIsuConditionTargetBaseURL, jiaIsuUUID}
	bodyJSON, err := json.Marshal(body)
	if err != nil {
		c.Logger().Error(err)
		fmt.Println("bad58")
		return c.NoContent(http.StatusInternalServerError)
	}

	reqJIA, err := http.NewRequest(http.MethodPost, targetURL, bytes.NewBuffer(bodyJSON))
	if err != nil {
		c.Logger().Error(err)
		fmt.Println("bad59")
		return c.NoContent(http.StatusInternalServerError)
	}

	reqJIA.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(reqJIA)
	if err != nil {
		c.Logger().Errorf("failed to request to JIAService: %v", err)
		fmt.Println("bad60")
		return c.NoContent(http.StatusInternalServerError)
	}
	defer res.Body.Close()

	resBody, err := ioutil.ReadAll(res.Body)
	if err != nil {
		c.Logger().Error(err)
		fmt.Println("bad61")
		return c.NoContent(http.StatusInternalServerError)
	}

	if res.StatusCode != http.StatusAccepted {
		c.Logger().Errorf("JIAService returned error: status code %v, message: %v", res.StatusCode, string(resBody))
		fmt.Println("bad62")
		return c.String(res.StatusCode, "JIAService returned error")
	}

	var isuFromJIA IsuFromJIA
	err = json.Unmarshal(resBody, &isuFromJIA)
	if err != nil {
		c.Logger().Error(err)
		fmt.Println("bad63")
		return c.NoContent(http.StatusInternalServerError)
	}

	_, err = tx.Exec("UPDATE `isu` SET `character` = ? WHERE  `jia_isu_uuid` = ?", isuFromJIA.Character, jiaIsuUUID)
	if err != nil {
		c.Logger().Errorf("db error: %v", err)
		fmt.Println("bad64")
		return c.NoContent(http.StatusInternalServerError)
	}

	var isu Isu
	err = tx.Get(
		&isu,
		"SELECT * FROM `isu` WHERE `jia_user_id` = ? AND `jia_isu_uuid` = ?",
		jiaUserID, jiaIsuUUID)
	if err != nil {
		c.Logger().Errorf("db error: %v", err)
		fmt.Println("bad65")
		return c.NoContent(http.StatusInternalServerError)
	}

	err = tx.Commit()
	if err != nil {
		c.Logger().Errorf("db error: %v", err)
		fmt.Println("bad66")
		return c.NoContent(http.StatusInternalServerError)
	}
	cacheGetIsuList.Forget(jiaUserID)
	cacheIsu.Forget(jiaIsuUUID)
	isuCountByIsuUUID.Forget(jiaIsuUUID)
	isuCache.Set([]Isu{isu})

	//fmt.Printf("PostIsu Success: %v\n", jiaIsuUUID)

	return c.JSON(http.StatusCreated, isu)
}

// GET /api/isu/:jia_isu_uuid
// ISUの情報を取得
func getIsuID(c echo.Context) error {
	jiaUserID, errStatusCode, err := getUserIDFromSession(c)
	if err != nil {
		if errStatusCode == http.StatusUnauthorized {
			fmt.Println("bad70")
			return c.String(http.StatusUnauthorized, "you are not signed in")
		}

		c.Logger().Error(err)
		fmt.Println("bad71")
		return c.NoContent(http.StatusInternalServerError)
	}

	jiaIsuUUID := c.Param("jia_isu_uuid")

	var res Isu
	//err = db.Get(&res, "SELECT * FROM `isu` WHERE `jia_user_id` = ? AND `jia_isu_uuid` = ?",
	//	jiaUserID, jiaIsuUUID)
	//if err != nil {
	//	if errors.Is(err, sql.ErrNoRows) {
	//		return c.String(http.StatusNotFound, "not found: isu")
	//	}
	//
	//	c.Logger().Errorf("db error: %v", err)
	//	return c.NoContent(http.StatusInternalServerError)
	//}
	var isu *Isu
	isu, err = cacheIsu.Get(context.Background(), jiaIsuUUID)
	//err = db.Get(&image, "SELECT `image` FROM `isu` WHERE `jia_user_id` = ? AND `jia_isu_uuid` = ?",
	//	jiaUserID, jiaIsuUUID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			fmt.Println("bad72")
			return c.String(http.StatusNotFound, "not found: isu")
		}
		c.Logger().Errorf("db error: %v", err)
		fmt.Println("bad73")
		return c.NoContent(http.StatusInternalServerError)
	}
	if isu.JIAUserID != jiaUserID {
		fmt.Println("bad74")
		return c.String(http.StatusNotFound, "not found: isu")
	}

	res = *isu

	return c.JSON(http.StatusOK, res)
}

// GET /api/isu/:jia_isu_uuid/icon
// ISUのアイコンを取得
func getIsuIcon(c echo.Context) error {
	jiaUserUUID, errStatusCode, err := getUserIDFromSession(c)
	if err != nil {
		if errStatusCode == http.StatusUnauthorized {
			fmt.Println("bad80")
			return c.String(http.StatusUnauthorized, "you are not signed in")
		}

		c.Logger().Error(err)
		fmt.Println("bad81")
		return c.NoContent(http.StatusInternalServerError)
	}

	jiaIsuUUID := c.Param("jia_isu_uuid")

	var isu *Isu
	isu, err = cacheIsu.Get(context.Background(), jiaIsuUUID)
	//err = db.Get(&image, "SELECT `image` FROM `isu` WHERE `jia_user_id` = ? AND `jia_isu_uuid` = ?",
	//	jiaUserID, jiaIsuUUID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			fmt.Println("bad82")
			return c.String(http.StatusNotFound, "not found: isu")
		}
		c.Logger().Errorf("db error: %v", err)
		fmt.Println("bad83")
		return c.NoContent(http.StatusInternalServerError)
	}
	if isu.JIAUserID != jiaUserUUID {
		fmt.Println("bad84")
		return c.String(http.StatusNotFound, "not found: isu")
	}

	//return c.Blob(http.StatusOK, "", isu.Image)

	c.Response().Header().Set("X-Accel-Redirect", "/isu-images/"+jiaIsuUUID)
	return c.NoContent(http.StatusOK)
}

// GET /api/isu/:jia_isu_uuid/graph
// ISUのコンディショングラフ描画のための情報を取得
func getIsuGraph(c echo.Context) error {
	jiaUserID, errStatusCode, err := getUserIDFromSession(c)
	if err != nil {
		if errStatusCode == http.StatusUnauthorized {
			fmt.Println("bad90")
			return c.String(http.StatusUnauthorized, "you are not signed in")
		}

		c.Logger().Error(err)
		fmt.Println("bad91")
		return c.NoContent(http.StatusInternalServerError)
	}

	jiaIsuUUID := c.Param("jia_isu_uuid")
	datetimeStr := c.QueryParam("datetime")
	if datetimeStr == "" {
		fmt.Println("bad92")
		return c.String(http.StatusBadRequest, "missing: datetime")
	}
	datetimeInt64, err := strconv.ParseInt(datetimeStr, 10, 64)
	if err != nil {
		fmt.Println("bad93")
		return c.String(http.StatusBadRequest, "bad format: datetime")
	}
	date := time.Unix(datetimeInt64, 0).Truncate(time.Hour)

	//tx, err := db.Beginx()
	if err != nil {
		c.Logger().Errorf("db error: %v", err)
		fmt.Println("bad94")
		return c.NoContent(http.StatusInternalServerError)
	}
	//defer tx.Rollback()

	//var count int
	//err = tx.Get(&count, "SELECT COUNT(*) FROM `isu` WHERE `jia_user_id` = ? AND `jia_isu_uuid` = ?",
	//	jiaUserID, jiaIsuUUID)
	//if err != nil {
	//	c.Logger().Errorf("db error: %v", err)
	//	return c.NoContent(http.StatusInternalServerError)
	//}
	//if count == 0 {
	//	return c.String(http.StatusNotFound, "not found: isu")
	//}

	isu, err := cacheIsu.Get(context.Background(), jiaIsuUUID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return c.String(http.StatusNotFound, "not found: isu")
		}

		c.Logger().Errorf("db error: %v", err)
		fmt.Println("bad95")
		return c.NoContent(http.StatusInternalServerError)
	}

	if isu.JIAUserID != jiaUserID {
		return c.String(http.StatusNotFound, "not found: isu")
	}

	res, err := generateIsuGraphResponse(jiaIsuUUID, date)
	if err != nil {
		c.Logger().Error(err)
		fmt.Println("bad96")
		return c.NoContent(http.StatusInternalServerError)
	}

	if err != nil {
		c.Logger().Errorf("db error: %v", err)
		fmt.Println("bad97")
		return c.NoContent(http.StatusInternalServerError)
	}

	return c.JSON(http.StatusOK, res)
}

// グラフのデータ点を一日分生成
func generateIsuGraphResponse(jiaIsuUUID string, graphDate time.Time) ([]GraphResponse, error) {
	dataPoints := []GraphDataPointWithInfo{}
	conditionsInThisHour := []IsuCondition{}
	timestampsInThisHour := []int64{}
	var startTimeInThisHour time.Time
	var condition IsuCondition

	rows, err := db.Queryx("SELECT * FROM `isu_condition` WHERE `jia_isu_uuid` = ? AND `timestamp` <= ? AND ? <= `timestamp` ORDER BY `timestamp` ASC", jiaIsuUUID, graphDate.Add(time.Hour*24), graphDate)
	if err != nil {
		fmt.Println("bad10")
		return nil, fmt.Errorf("db error: %v", err)
	}

	for rows.Next() {
		err = rows.StructScan(&condition)
		if err != nil {
			fmt.Println("bad11")
			return nil, err
		}

		truncatedConditionTime := condition.Timestamp.Truncate(time.Hour)
		if truncatedConditionTime != startTimeInThisHour {
			if len(conditionsInThisHour) > 0 {
				data, err := calculateGraphDataPoint(conditionsInThisHour)
				if err != nil {
					fmt.Println("bad12")
					return nil, err
				}

				dataPoints = append(dataPoints,
					GraphDataPointWithInfo{
						JIAIsuUUID:          jiaIsuUUID,
						StartAt:             startTimeInThisHour,
						Data:                data,
						ConditionTimestamps: timestampsInThisHour})
			}

			startTimeInThisHour = truncatedConditionTime
			conditionsInThisHour = []IsuCondition{}
			timestampsInThisHour = []int64{}
		}
		conditionsInThisHour = append(conditionsInThisHour, condition)
		timestampsInThisHour = append(timestampsInThisHour, condition.Timestamp.Unix())
	}

	if len(conditionsInThisHour) > 0 {
		data, err := calculateGraphDataPoint(conditionsInThisHour)
		if err != nil {
			fmt.Println("bad13")
			return nil, err
		}

		dataPoints = append(dataPoints,
			GraphDataPointWithInfo{
				JIAIsuUUID:          jiaIsuUUID,
				StartAt:             startTimeInThisHour,
				Data:                data,
				ConditionTimestamps: timestampsInThisHour})
	}

	endTime := graphDate.Add(time.Hour * 24)
	startIndex := len(dataPoints)
	endNextIndex := len(dataPoints)
	for i, graph := range dataPoints {
		if startIndex == len(dataPoints) && !graph.StartAt.Before(graphDate) {
			startIndex = i
		}
		if endNextIndex == len(dataPoints) && graph.StartAt.After(endTime) {
			endNextIndex = i
		}
	}

	filteredDataPoints := []GraphDataPointWithInfo{}
	if startIndex < endNextIndex {
		filteredDataPoints = dataPoints[startIndex:endNextIndex]
	}

	responseList := []GraphResponse{}
	index := 0
	thisTime := graphDate

	for thisTime.Before(graphDate.Add(time.Hour * 24)) {
		var data *GraphDataPoint
		timestamps := []int64{}

		if index < len(filteredDataPoints) {
			dataWithInfo := filteredDataPoints[index]

			if dataWithInfo.StartAt.Equal(thisTime) {
				data = &dataWithInfo.Data
				timestamps = dataWithInfo.ConditionTimestamps
				index++
			}
		}

		resp := GraphResponse{
			StartAt:             thisTime.Unix(),
			EndAt:               thisTime.Add(time.Hour).Unix(),
			Data:                data,
			ConditionTimestamps: timestamps,
		}
		responseList = append(responseList, resp)

		thisTime = thisTime.Add(time.Hour)
	}

	return responseList, nil
}

// 複数のISUのコンディションからグラフの一つのデータ点を計算
func calculateGraphDataPoint(isuConditions []IsuCondition) (GraphDataPoint, error) {
	conditionsCount := map[string]int{"is_broken": 0, "is_dirty": 0, "is_overweight": 0}
	rawScore := 0
	for _, condition := range isuConditions {
		badConditionsCount := 0

		if !isValidConditionFormat(condition.Condition) {
			fmt.Println("bad14")
			return GraphDataPoint{}, fmt.Errorf("invalid condition format")
		}

		for _, condStr := range strings.Split(condition.Condition, ",") {
			keyValue := strings.Split(condStr, "=")

			conditionName := keyValue[0]
			if keyValue[1] == "true" {
				conditionsCount[conditionName] += 1
				badConditionsCount++
			}
		}

		if badConditionsCount >= 3 {
			rawScore += scoreConditionLevelCritical
		} else if badConditionsCount >= 1 {
			rawScore += scoreConditionLevelWarning
		} else {
			rawScore += scoreConditionLevelInfo
		}
	}

	sittingCount := 0
	for _, condition := range isuConditions {
		if condition.IsSitting {
			sittingCount++
		}
	}

	isuConditionsLength := len(isuConditions)

	score := rawScore * 100 / 3 / isuConditionsLength

	sittingPercentage := sittingCount * 100 / isuConditionsLength
	isBrokenPercentage := conditionsCount["is_broken"] * 100 / isuConditionsLength
	isOverweightPercentage := conditionsCount["is_overweight"] * 100 / isuConditionsLength
	isDirtyPercentage := conditionsCount["is_dirty"] * 100 / isuConditionsLength

	dataPoint := GraphDataPoint{
		Score: score,
		Percentage: ConditionsPercentage{
			Sitting:      sittingPercentage,
			IsBroken:     isBrokenPercentage,
			IsOverweight: isOverweightPercentage,
			IsDirty:      isDirtyPercentage,
		},
	}
	return dataPoint, nil
}

// GET /api/condition/:jia_isu_uuid
// ISUのコンディションを取得
func getIsuConditions(c echo.Context) error {
	//fmt.Println("getIsuConditions Requested")
	jiaUserID, errStatusCode, err := getUserIDFromSession(c)
	if err != nil {
		if errStatusCode == http.StatusUnauthorized {
			return c.String(http.StatusUnauthorized, "you are not signed in")
		}

		c.Logger().Error(err)
		fmt.Println("bad20")
		return c.NoContent(http.StatusInternalServerError)
	}

	jiaIsuUUID := c.Param("jia_isu_uuid")
	if jiaIsuUUID == "" {
		fmt.Println("bad21")
		return c.String(http.StatusBadRequest, "missing: jia_isu_uuid")
	}

	endTimeInt64, err := strconv.ParseInt(c.QueryParam("end_time"), 10, 64)
	if err != nil {
		fmt.Println("bad22")
		return c.String(http.StatusBadRequest, "bad format: end_time")
	}
	endTime := time.Unix(endTimeInt64, 0)
	conditionLevelCSV := c.QueryParam("condition_level")
	if conditionLevelCSV == "" {
		fmt.Println("bad23")
		return c.String(http.StatusBadRequest, "missing: condition_level")
	}
	conditionLevel := map[string]interface{}{}
	for _, level := range strings.Split(conditionLevelCSV, ",") {
		conditionLevel[level] = struct{}{}
	}

	startTimeStr := c.QueryParam("start_time")
	var startTime time.Time
	if startTimeStr != "" {
		startTimeInt64, err := strconv.ParseInt(startTimeStr, 10, 64)
		if err != nil {
			fmt.Println("bad24")
			return c.String(http.StatusBadRequest, "bad format: start_time")
		}
		startTime = time.Unix(startTimeInt64, 0)
	}

	var isuName string
	isu, err := cacheIsu.Get(context.Background(), jiaIsuUUID)
	//err = db.Get(&isuName,
	//	"SELECT name FROM `isu` WHERE `jia_isu_uuid` = ? AND `jia_user_id` = ? LIMIT 1",
	//	jiaIsuUUID, jiaUserID,
	//)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			fmt.Println("bad25")
			return c.String(http.StatusNotFound, "not found: isu")
		}

		c.Logger().Errorf("db error: %v", err)
		fmt.Println("bad26")
		return c.NoContent(http.StatusInternalServerError)
	}
	if isu.JIAUserID != jiaUserID {
		fmt.Println("bad27")
		return c.String(http.StatusNotFound, "not found: isu")
	}
	isuName = isu.Name

	conditionsResponse, err := getIsuConditionsFromDB(db, jiaIsuUUID, endTime, conditionLevel, startTime, conditionLimit, isuName)
	if err != nil {
		c.Logger().Errorf("db error: %v", err)
		fmt.Println("bad28")
		return c.NoContent(http.StatusInternalServerError)
	}

	//fmt.Println("getIsuConditions Success")
	return c.JSON(http.StatusOK, conditionsResponse)
}

// ISUのコンディションをDBから取得
func getIsuConditionsFromDB(db *sqlx.DB, jiaIsuUUID string, endTime time.Time, conditionLevel map[string]interface{}, startTime time.Time,
	limit int, isuName string) ([]*GetIsuConditionResponse, error) {

	var conditions []GetIsuCondition
	var err error

	var allConditionLevel bool
	if _, ok1 := conditionLevel[conditionLevelInfo]; ok1 {
		if _, ok2 := conditionLevel[conditionLevelWarning]; ok2 {
			if _, ok3 := conditionLevel[conditionLevelCritical]; ok3 {
				allConditionLevel = true
			}
		}
	}
	if allConditionLevel {
		if startTime.IsZero() {
			err = db.Select(&conditions,
				"SELECT * FROM `isu_condition` WHERE `jia_isu_uuid` = ?"+
					"	AND `timestamp` < ?"+
					"	ORDER BY `timestamp` DESC LIMIT ?",
				jiaIsuUUID, endTime, limit,
			)
		} else {
			err = db.Select(&conditions,
				"SELECT * FROM `isu_condition` WHERE `jia_isu_uuid` = ?"+
					"	AND `timestamp` < ?"+
					"	AND ? <= `timestamp`"+
					"	ORDER BY `timestamp` DESC LIMIT ?",
				jiaIsuUUID, endTime, startTime, limit,
			)
		}
	} else {
		conditionLevelQuery := ""
		moreThanOne := false
		if _, ok := conditionLevel[conditionLevelInfo]; ok {
			conditionLevelQuery += "'" + conditionLevelInfo + "'"
			moreThanOne = true
		}
		if _, ok := conditionLevel[conditionLevelWarning]; ok {
			if moreThanOne {
				conditionLevelQuery += ","
			}
			conditionLevelQuery += "'" + conditionLevelWarning + "'"
			moreThanOne = true
		}
		if _, ok := conditionLevel[conditionLevelCritical]; ok {
			if moreThanOne {
				conditionLevelQuery += ","
			}
			conditionLevelQuery += "'" + conditionLevelCritical + "'"
			moreThanOne = true
		}

		if startTime.IsZero() {
			err = db.Select(&conditions,
				"SELECT * FROM `isu_condition` WHERE `jia_isu_uuid` = ?"+
					"	AND `timestamp` < ?"+
					"   AND `condition_level` IN ("+conditionLevelQuery+")"+
					"	ORDER BY `timestamp` DESC LIMIT ?",
				jiaIsuUUID, endTime, limit,
			)
		} else {
			err = db.Select(&conditions,
				"SELECT * FROM `isu_condition` WHERE `jia_isu_uuid` = ?"+
					"	AND `timestamp` < ?"+
					"	AND ? <= `timestamp`"+
					"   AND `condition_level` IN ("+conditionLevelQuery+")"+
					"	ORDER BY `timestamp` DESC LIMIT ?",
				jiaIsuUUID, endTime, startTime, limit,
			)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("db error: %v", err)
	}

	var conditionsResponse []*GetIsuConditionResponse
	for _, c := range conditions {
		data := GetIsuConditionResponse{
			JIAIsuUUID:     c.JIAIsuUUID,
			IsuName:        isuName,
			Timestamp:      c.Timestamp.Unix(),
			IsSitting:      c.IsSitting,
			Condition:      c.Condition,
			ConditionLevel: c.ConditionLevel,
			Message:        c.Message,
		}
		conditionsResponse = append(conditionsResponse, &data)
	}

	if len(conditionsResponse) > limit {
		conditionsResponse = conditionsResponse[:limit]
	}

	return conditionsResponse, nil
}

// ISUのコンディションの文字列からコンディションレベルを計算
func calculateConditionLevel(condition string) (string, error) {
	var conditionLevel string

	warnCount := strings.Count(condition, "=true")
	switch warnCount {
	case 0:
		conditionLevel = conditionLevelInfo
	case 1, 2:
		conditionLevel = conditionLevelWarning
	case 3:
		conditionLevel = conditionLevelCritical
	default:
		fmt.Println("bad29")
		return "", fmt.Errorf("unexpected warn count")
	}

	return conditionLevel, nil
}

var trendResponse []TrendResponse

// GET /api/trend
// ISUの性格毎の最新のコンディション情報
func getTrend(c echo.Context) error {
	return c.JSON(http.StatusOK, trendResponse)
}

type PostIsuConditionRequests struct {
	JiaIsuUUID     string `json:"jia_isu_uuid"`
	Timestamp      int64  `json:"timestamp"`
	IsSitting      bool   `json:"is_sitting"`
	Condition      string `json:"condition"`
	Message        string `json:"message"`
	ConditionLevel string `json:"condition_level"`
}

var postIsuConditionRequests []PostIsuConditionRequests

// POST /api/condition/:jia_isu_uuid
// ISUからのコンディションを受け取る
func postIsuCondition(c echo.Context) error {
	//fmt.Println("PostIsuCondition Requested")
	// TODO: 一定割合リクエストを落としてしのぐようにしたが、本来は全量さばけるようにすべき
	//dropProbability := 0.5
	//if rand.Float64() <= dropProbability {
	//	c.Logger().Warnf("drop post isu condition request")
	//	return c.NoContent(http.StatusAccepted)
	//}

	jiaIsuUUID := c.Param("jia_isu_uuid")
	if jiaIsuUUID == "" {
		fmt.Println("bad1")
		return c.String(http.StatusBadRequest, "missing: jia_isu_uuid")
	}
	var req []PostIsuConditionRequest
	err := c.Bind(&req)
	if err != nil {
		fmt.Println("bad2")
		return c.String(http.StatusBadRequest, "bad request body")
	} else if len(req) == 0 {
		fmt.Println("bad3")
		return c.String(http.StatusBadRequest, "bad request body")
	}
	for _, cond := range req {
		if !isValidConditionFormat(cond.Condition) {
			fmt.Println("bad4")
			return c.String(http.StatusBadRequest, "bad request body")
		}
	}
	//tx, err := db.Beginx()
	//if err != nil {
	//	c.Logger().Errorf("db error: %v", err)
	//	return c.NoContent(http.StatusInternalServerError)
	//}
	//defer tx.Rollback()
	var count *int
	// default: tx
	count, err = isuCountByIsuUUID.Get(context.Background(), jiaIsuUUID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			fmt.Println("bad6")
			return c.String(http.StatusNotFound, "not found: isu")
		}
		if errors.Is(err, notFound) {
			fmt.Println("bad8")
			return c.String(http.StatusNotFound, "not found: isu")
		}
		fmt.Println("bad5")
		c.Logger().Errorf("db error: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}
	if *count == 0 {
		fmt.Println("bad7")
		return c.String(http.StatusNotFound, "not found: isu")
	}

	for _, r := range req {
		conditionLevel, err := calculateConditionLevel(r.Condition)
		if err != nil {
			fmt.Println("bad7")
			return c.NoContent(http.StatusInternalServerError)
		}

		appendRequest := PostIsuConditionRequests{
			JiaIsuUUID:     jiaIsuUUID,
			Timestamp:      r.Timestamp,
			IsSitting:      r.IsSitting,
			Condition:      r.Condition,
			ConditionLevel: conditionLevel,
			Message:        r.Message,
		}
		postIsuConditionRequests = append(postIsuConditionRequests, appendRequest)
	}

	//fmt.Println("PostIsuCondition Success: ", postIsuConditionRequests)
	//fmt.Printf("PostIsuCondition Success: %v\n", len(postIsuConditionRequests))
	return c.NoContent(http.StatusAccepted)
}

// ISUのコンディションの文字列がcsv形式になっているか検証
func isValidConditionFormat(conditionStr string) bool {

	keys := []string{"is_dirty=", "is_overweight=", "is_broken="}
	const valueTrue = "true"
	const valueFalse = "false"

	idxCondStr := 0

	for idxKeys, key := range keys {
		if !strings.HasPrefix(conditionStr[idxCondStr:], key) {
			return false
		}
		idxCondStr += len(key)

		if strings.HasPrefix(conditionStr[idxCondStr:], valueTrue) {
			idxCondStr += len(valueTrue)
		} else if strings.HasPrefix(conditionStr[idxCondStr:], valueFalse) {
			idxCondStr += len(valueFalse)
		} else {
			return false
		}

		if idxKeys < (len(keys) - 1) {
			if conditionStr[idxCondStr] != ',' {
				return false
			}
			idxCondStr++
		}
	}

	return (idxCondStr == len(conditionStr))
}

func getIndex(c echo.Context) error {
	return c.File(frontendContentsPath + "/index.html")
}
