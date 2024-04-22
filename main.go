package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/mdaxf/iac/com"
	"github.com/mdaxf/iac/config"
	dbconn "github.com/mdaxf/iac/databases"
	"github.com/mdaxf/iac/documents"
	"github.com/mdaxf/iac/engine/trancode"
	"github.com/mdaxf/iac/framework/callback_mgr"
	"github.com/mdaxf/iac/health"
	"github.com/mdaxf/iac/integration/kafka"
	iacmb "github.com/mdaxf/iac/integration/signalr"
	"github.com/mdaxf/iac/logger"
	"github.com/mdaxf/signalrsrv/signalr"
)

var (
	nodedata       map[string]interface{}
	nodecomponent  map[string]interface{}
	monitorPort    int
	monitorServer  *http.Server
	KafkaConsumers []*kafka.KafkaConsumer
)

func main() {
	startTime := time.Now()
	var wg sync.WaitGroup
	nodecomponent = make(map[string]interface{})

	gconfig, err := config.LoadGlobalConfig()

	if err != nil {
		log.Fatalf("Failed to load global configuration: %v", err)
		//ilog.Error(fmt.Sprintf("Failed to load global configuration: %v", err))
	}
	initializeloger(gconfig)

	AppID := uuid.New().String()
	nodedata = make(map[string]interface{})
	nodedata["Name"] = "iac-kafka"
	nodedata["AppID"] = AppID
	nodedata["Description"] = "IAC Kafka Client Service"
	nodedata["Type"] = "Kafka"
	nodedata["Version"] = "1.0.0"
	nodedata["Status"] = "Running"
	nodedata["StartTime"] = time.Now().UTC()

	ilog := logger.Log{ModuleName: logger.Framework, User: "System", ControllerName: "iac-mqtt"}
	ilog.Debug("Start the iac-mqtt")

	DB := initializeDatabase(ilog, gconfig)
	if DB == nil {
		ilog.Error("Database connection error")
		return
	}

	docDB := initializedDocuments(ilog, gconfig)
	if docDB == nil {
		ilog.Error("MongoDB connection error!")
		return
	}

	IACMessageBusClient, err := iacmb.Connect(gconfig.SingalRConfig)
	if err != nil {
		ilog.Error(fmt.Sprintf("Failed to connect to IAC Message Bus: %v", err))
	} else {
		ilog.Debug(fmt.Sprintf("IAC Message Bus: %v", IACMessageBusClient))
	}

	nodecomponent["DB"] = DB
	nodecomponent["DocDB"] = docDB
	nodecomponent["IACMessageBusClient"] = IACMessageBusClient

	if callback_mgr.CallBackMap["TranCode_Execute"] == nil {
		ilog.Debug("Register the trancode execution interface")
		tfr := trancode.TranFlowstr{}
		callback_mgr.RegisterCallBack("TranCode_Execute", tfr.Execute)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		initializeKafka(gconfig, ilog, DB, docDB, IACMessageBusClient)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		startMonitorServer(ilog, gconfig)
	}()

	// Start the HeartBeat
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			HeartBeat(ilog, gconfig, DB, docDB, IACMessageBusClient)
			time.Sleep(5 * time.Minute)
		}
	}()

	elapsed := time.Since(startTime)
	ilog.PerformanceWithDuration("iac-mqtt.main", elapsed)
	wg.Wait()

	waitForTerminationSignal(ilog, gconfig)
}

func HeartBeat(ilog logger.Log, gconfig *config.GlobalConfig, DB *sql.DB, DocDB *documents.DocDB, IACMessageBusClient signalr.Client) {
	ilog.Debug("Start HeartBeat for iac-activemq application with appid: " + nodedata["AppID"].(string))
	appHeartBeatUrl := com.ConverttoString(gconfig.AppServer["url"]) + "/IACComponents/heartbeat"
	ilog.Debug("HeartBeat URL: " + appHeartBeatUrl)

	result, err := health.CheckNodeHealth(nodedata, DB, DocDB.MongoDBClient, IACMessageBusClient)

	ilog.Debug(fmt.Sprintf("HeartBeat Result: %v", result))

	serviceresult, err := CheckServiceStatus(ilog)
	if err != nil {
		ilog.Error(fmt.Sprintf("HeartBeat error: %v", err))
	}

	data := make(map[string]interface{})
	data["Node"] = nodedata
	data["Result"] = result
	data["ServiceStatus"] = serviceresult
	data["timestamp"] = time.Now().UTC()
	// send the heartbeat to the server
	headers := make(map[string]string)
	headers["Content-Type"] = "application/json"
	headers["Authorization"] = "apikey " + com.ConverttoString(gconfig.AppServer["apikey"])

	response, err := com.CallWebService(appHeartBeatUrl, "POST", data, headers)

	if err != nil {
		ilog.Error(fmt.Sprintf("HeartBeat error: %v", err))
		return
	}

	ilog.Debug(fmt.Sprintf("HeartBeat post response: %v", response))
}

func CheckServiceStatus(iLog logger.Log) (map[string]interface{}, error) {
	iLog.Debug("Check ActiveMQ Status")
	result := make(map[string]interface{})
	OKCount := 0
	UnavailableCount := 0
	for _, consumer := range KafkaConsumers {
		if consumer.Consumer != nil {
			result[consumer.Config.Server] = health.StatusOK
			OKCount = OKCount + 1
		} else {
			result[consumer.Config.Server] = health.StatusUnavailable
			UnavailableCount = UnavailableCount + 1
		}
	}
	OverAllStatus := health.StatusOK

	if OKCount == 0 {
		OverAllStatus = health.StatusUnavailable
	} else if UnavailableCount > 0 && OKCount > 0 {
		OverAllStatus = health.StatusPartiallyAvailable
	} else {
		OverAllStatus = health.StatusOK
	}
	result["OverallStatus"] = OverAllStatus
	return result, nil
}

func initializeloger(gconfig *config.GlobalConfig) error {
	if gconfig.LogConfig == nil {
		return fmt.Errorf("log configuration is missing")
	}
	fmt.Printf("log configuration: %v", gconfig.LogConfig)
	logger.Init(gconfig.LogConfig)
	return nil
}

func initializeKafka(gconfig *config.GlobalConfig, ilog logger.Log, DB *sql.DB, DocDB *documents.DocDB, IACMessageBusClient signalr.Client) {
	startTime := time.Now()
	defer func() {
		elapsed := time.Since(startTime)
		ilog.PerformanceWithDuration("main.initializeKafka", elapsed)
	}()

	go func() {

		ilog.Debug("initialize Kafka Connection")

		data, err := ioutil.ReadFile("kafkaconfig.json")
		if err != nil {
			ilog.Debug(fmt.Sprintf("failed to read configuration file: %v", err))
			return
		}
		ilog.Debug(fmt.Sprintf("Kafka conenction configuration file: %s", string(data)))
		var kafkacfgs kafka.KafkasConfig

		err = json.Unmarshal(data, &kafkacfgs)
		if err != nil {
			ilog.Debug(fmt.Sprintf("failed to unmarshal the configuration file: %v", err))

		}
		ilog.Debug(fmt.Sprintf("Kafka Connection configuration: %v", logger.ConvertJson(kafkacfgs)))

		for _, kafakacfg := range kafkacfgs.Kafkas {
			ilog.Debug(fmt.Sprintf("Single Kafka Connection configuration: %s", logger.ConvertJson(kafakacfg)))
			kafkacon := kafka.NewKafkaConsumerExternal(kafakacfg, DocDB, DB, IACMessageBusClient)
			kafkacon.AppServer = com.ConverttoString(gconfig.AppServer["url"])
			kafkacon.ApiKey = kafkacfgs.ApiKey
			KafkaConsumers = append(KafkaConsumers, kafkacon)
			kafkacon.BuildKafkaConsumer()
		}
	}()

}

func waitForTerminationSignal(ilog logger.Log, gconfig *config.GlobalConfig) {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c
	fmt.Println("\nShutting down...")
	if nodecomponent["DB"].(*sql.DB) != nil {
		nodecomponent["DB"].(*sql.DB).Close()
	}

	if nodecomponent["DocDB"].(*documents.DocDB) != nil {
		nodecomponent["DocDB"].(*documents.DocDB).MongoDBClient.Disconnect(nil)
	}

	if nodecomponent["IACMessageBusClient"].(signalr.Client) != nil {
		nodecomponent["IACMessageBusClient"].(signalr.Client).Stop()
	}

	ilog.Debug("Start HeartBeat for iac-activemq application with appid: " + nodedata["AppID"].(string))
	appHeartBeatUrl := com.ConverttoString(gconfig.AppServer["url"]) + "/IACComponents/close"
	ilog.Debug("HeartBeat URL: " + appHeartBeatUrl)

	data := make(map[string]interface{})
	data["Node"] = nodedata
	data["timestamp"] = time.Now().UTC()

	headers := make(map[string]string)
	headers["Content-Type"] = "application/json"
	headers["Authorization"] = "apikey " + com.ConverttoString(gconfig.AppServer["apikey"])

	_, err := com.CallWebService(appHeartBeatUrl, "POST", data, headers)

	if err != nil {
		ilog.Error(fmt.Sprintf("HeartBeat error: %v", err))
	}
	go func() {
		if nodecomponent["DB"].(*sql.DB) != nil {
			nodecomponent["DB"].(*sql.DB).Close()
		}

		if nodecomponent["DocDB"].(*documents.DocDB) != nil {
			nodecomponent["DocDB"].(*documents.DocDB).MongoDBClient.Disconnect(nil)
		}

		if nodecomponent["IACMessageBusClient"].(signalr.Client) != nil {
			nodecomponent["IACMessageBusClient"].(signalr.Client).Stop()
		}
	}()

	if monitorServer != nil {
		monitorServer.Shutdown(context.Background())
	}
	time.Sleep(2 * time.Second) // Add any cleanup or graceful shutdown logic here
	os.Exit(0)
}

func initializeDatabase(ilog logger.Log, gconfig *config.GlobalConfig) *sql.DB {
	// function execution start time
	startTime := time.Now()

	// defer function to log the performance duration of initializeDatabase
	defer func() {
		elapsed := time.Since(startTime)
		ilog.PerformanceWithDuration("main.initializeDatabase", elapsed)
	}()

	ilog.Debug("initialize Database")
	databaseconfig := gconfig.DatabaseConfig

	// check if database type is missing
	if databaseconfig["type"] == nil {
		ilog.Error(fmt.Sprintf("initialize Database error: %s", "DatabaseType is missing"))
		return nil
	}

	// check if database connection is missing
	if databaseconfig["connection"] == nil {
		ilog.Error(fmt.Sprintf("initialize Database error: %s", "DatabaseConnection is missing"))
		return nil
	}

	// set the database type and connection string
	dbconn.DatabaseType = databaseconfig["type"].(string)
	dbconn.DatabaseConnection = databaseconfig["connection"].(string)

	// set the maximum idle connections, default to 5 if not provided or if the value is not a float64
	if databaseconfig["maxidleconns"] == nil {
		dbconn.MaxIdleConns = 5
	} else {
		if v, ok := databaseconfig["maxidleconns"].(float64); ok {
			dbconn.MaxIdleConns = int(v)
		} else {
			dbconn.MaxIdleConns = 5
		}
	}

	// set the maximum open connections, default to 10 if not provided or if the value is not a float64
	if databaseconfig["maxopenconns"] == nil {
		dbconn.MaxOpenConns = 10
	} else {
		if v, ok := databaseconfig["maxopenconns"].(float64); ok {
			dbconn.MaxOpenConns = int(v)
		} else {
			dbconn.MaxOpenConns = 10
		}
	}

	// connect to the database
	err := dbconn.ConnectDB()
	if err != nil {
		ilog.Error(fmt.Sprintf("initialize Database error: %s", err.Error()))
		return nil
	}

	return dbconn.DB
}

func initializedDocuments(ilog logger.Log, gconfig *config.GlobalConfig) *documents.DocDB {
	startTime := time.Now()
	defer func() {
		elapsed := time.Since(startTime)
		ilog.PerformanceWithDuration("main.initializedDocuments", elapsed)
	}()

	if gconfig.DocumentConfig == nil {
		fmt.Errorf("documentdb configuration is missing")
		return nil
	}

	documentsConfig := gconfig.DocumentConfig

	ilog.Debug(fmt.Sprintf("initialize Documents, %v", documentsConfig))

	var DatabaseType = documentsConfig["type"].(string)             // "mongodb"
	var DatabaseConnection = documentsConfig["connection"].(string) //"mongodb://localhost:27017"
	var DatabaseName = documentsConfig["database"].(string)         //"IAC_CFG"

	if DatabaseType == "" {
		ilog.Error(fmt.Sprintf("initialize Documents error: %s", "DatabaseType is missing"))
		return nil
	}
	if DatabaseConnection == "" {
		ilog.Error(fmt.Sprintf("initialize Documents error: %s", "DatabaseConnection is missing"))
		return nil
	}
	if DatabaseName == "" {
		ilog.Error(fmt.Sprintf("initialize Documents error: %s", "DatabaseName is missing"))
		return nil
	}

	documents.ConnectDB(DatabaseType, DatabaseConnection, DatabaseName)

	return documents.DocDBCon
}

func startMonitorServer(ilog logger.Log, gconfig *config.GlobalConfig) {
	err := error(nil)
	monitorPort, err = getAvailablePort(8800, 8900)
	if err != nil {
		ilog.Error(fmt.Sprintf("Failed to get an available port: %v", err))
	}
	ilog.Debug(fmt.Sprintf("Starting monitor server on port %d", monitorPort))
	nodedata["MonitorPort"] = monitorPort
	// Start the monitor server
	hip, err := com.GetHostandIPAddress()

	if err != nil {
		ilog.Error(fmt.Sprintf("Failed to get host and ip address: %v", err))
	}
	for key, value := range hip {
		nodedata[key] = value
	}
	if hip["Host"] != nil {
		nodedata["healthapi"] = fmt.Sprintf("http://%s:%d/health", hip["Host"], monitorPort)
	}
	monitorServer = &http.Server{Addr: fmt.Sprintf(":%d", monitorPort)}
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if r.Header.Get("Authorization") != "apikey "+gconfig.AppServer["apikey"].(string) {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		result, err := health.CheckNodeHealth(nodedata, nodecomponent["DB"].(*sql.DB), nodecomponent["DocDB"].(*documents.DocDB).MongoDBClient, nodecomponent["IACMessageBusClient"].(signalr.Client))

		ilog.Debug(fmt.Sprintf("Health Result: %v", result))

		activemqsresult, err := CheckServiceStatus(ilog)
		if err != nil {
			ilog.Error(fmt.Sprintf("HeartBeat error: %v", err))
		}

		data := make(map[string]interface{})
		data["Node"] = nodedata
		data["Result"] = result
		data["ServiceStatus"] = activemqsresult
		data["time"] = time.Now().UTC()

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(data)
	})
	http.HandleFunc("/reloadconfig", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if r.Header.Get("Authorization") != "apikey "+gconfig.AppServer["apikey"].(string) {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		ilog.Debug("Reloading configuration - close the connections and reinitialize the components")
		/*	nodecomponent["DB"].(*sql.DB).Close()
			nodecomponent["DocDB"].(*documents.DocDB).MongoDBClient.Disconnect(nil)
			nodecomponent["IACMessageBusClient"].(signalr.Client).Stop()
		*/
		initializeKafka(gconfig, ilog, nodecomponent["DB"].(*sql.DB), nodecomponent["DocDB"].(*documents.DocDB), nodecomponent["IACMessageBusClient"].(signalr.Client))

		w.Header().Set("Content-Type", "application/json")
		data := make(map[string]interface{})
		data["Status"] = "Success"
		json.NewEncoder(w).Encode(data)
	})
	ilog.Debug(fmt.Sprintf("Starting server on port %d", monitorPort))
	err = monitorServer.ListenAndServe()
	if err != nil && err != http.ErrServerClosed {
		ilog.Error(fmt.Sprintf("Failed to start server: %v", err))
	}

}

func getAvailablePort(minPort, maxPort int) (int, error) {
	for port := minPort; port <= maxPort; port++ {
		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
		if err == nil {
			ln.Close()
			return port, nil
		}
	}
	return 0, fmt.Errorf("no available port found in the range %d-%d", minPort, maxPort)
}
