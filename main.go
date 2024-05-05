package main

import (
	"database/sql"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net/http"
	"strconv"
	"time"

	"github.com/canonical/lxd/shared/api"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	_ "github.com/mattn/go-sqlite3"
	"golang.org/x/crypto/bcrypt"
)

var client *LXCClient
var db *sql.DB

func home(c *gin.Context) {
	user := c.MustGet(gin.AuthUserKey).(string)
	containers, err := client.ListContainers(user)
	sshPorts := make(map[string]int)
	for _, container := range containers {
		sshPorts[container.Name] = client.SSHPort(container.Name)
	}
	if err != nil {
		c.Error(err)
		return
	}
	c.HTML(200, "home.html", gin.H{
		"containers": containers,
		"sshPorts":   sshPorts,
		"user":       user,
	})
}

func signup(c *gin.Context) {
	if c.Request.Method == "GET" {
		c.HTML(200, "signup.html", nil)
	} else {
		username := c.PostForm("username")
		password := c.PostForm("password")
		hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			c.Error(err)
			return
		}
		// check if user exists
		rows, err := db.Query("SELECT username FROM users WHERE username = ?", username)
		if err != nil {
			c.Error(err)
			return
		}
		if rows.Next() {
			c.HTML(200, "signup.html", gin.H{
				"error": "user already exists",
			})
			return
		}
		err = rows.Close()
		if err != nil {
			c.Error(err)
			return
		}
		_, err = db.Exec("INSERT INTO users (username, password, instance_limit) VALUES (?, ?, 3)", username, string(hash))
		if err != nil {
			c.Error(err)
			println("error", err)
			return
		}
		c.Redirect(302, "/")
	}
}

func create(c *gin.Context) {
	user := c.MustGet(gin.AuthUserKey).(string)
	friendlyname := c.PostForm("friendlyname")
	err := client.CreateContainer(user, friendlyname)
	if err != nil {
		c.Error(err)
		return
	}
	c.Redirect(302, "/")
}

func start(c *gin.Context) {
	user := c.MustGet(gin.AuthUserKey).(string)
	name := c.Param("name")
	err := client.StartContainer(user, name)
	if err != nil {
		c.Error(err)
		return
	}
	c.Status(200)
}

func stop(c *gin.Context) {
	user := c.MustGet(gin.AuthUserKey).(string)
	name := c.Param("name")
	err := client.StopContainer(user, name)
	if err != nil {
		c.Error(err)
		return
	}
	c.Status(200)
}

func delete(c *gin.Context) {
	user := c.MustGet(gin.AuthUserKey).(string)
	name := c.Param("name")
	err := client.DeleteContainer(user, name)
	if err != nil {
		c.Error(err)
		return
	}
	c.Status(200)
}

func shell(c *gin.Context) {
	user := c.MustGet(gin.AuthUserKey).(string)
	name := c.Param("name")
	container, err := client.GetContainer(user, name)
	if err != nil {
		c.Error(err)
		return
	}
	if container == nil {
		c.Redirect(302, "/")
		return
	}
	c.HTML(200, "shell.html", gin.H{
		"container": container,
	})
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  10240,
	WriteBufferSize: 10240,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

func terminal(c *gin.Context) {
	user := c.MustGet(gin.AuthUserKey).(string)
	name := c.Param("name")
	container, err := client.GetContainer(user, name)
	if err != nil {
		c.Error(err)
		return
	}
	if container == nil {
		c.JSON(400, gin.H{
			"error": "container not found",
		})
		return
	}
	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		c.Error(err)
		return
	}
	conn.SetCloseHandler(func(code int, text string) error {
		println("close", code, text)
		return nil
	})
	defer conn.Close()
	stdinReader, stdinWriter := io.Pipe()
	stdoutReader, stdoutWriter := io.Pipe()
	ch := make(chan api.InstanceExecControl)

	go func() {
		for {
			conn.SetReadDeadline(time.Now().Add(10 * time.Second))
			messageType, message, err := conn.ReadMessage()
			if err != nil {
				ch <- api.InstanceExecControl{
					Command: "signal",
					Signal:  9,
				}
				return
			}
			if messageType == websocket.TextMessage {
				stdinWriter.Write(message)
			} else {
				var msg api.InstanceExecControl
				if err := json.Unmarshal(message, &msg); err != nil {
					continue
				}
				fmt.Printf("msg: %+v\n", msg)
				ch <- msg
			}
		}
	}()
	go func() {
		for {
			data := make([]byte, 10240)
			n, err := stdoutReader.Read(data)
			if err != nil {
				return
			}
			if n > 0 {
				conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
				conn.WriteMessage(websocket.BinaryMessage, data[:n])
			}
		}
	}()
	err = client.StartShell(name, stdinReader, stdoutWriter, ch)
	if err != nil {
		c.Error(err)
		return
	}
}

func BasicAuth(c *gin.Context) {
	realm := "Authorization Required"
	realm = "Basic realm=" + strconv.Quote(realm)
	user, password, ok := c.Request.BasicAuth()
	if !ok {
		c.Header("WWW-Authenticate", realm)
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}
	rows, err := db.Query("SELECT username, password FROM users WHERE username = ?", user)
	if err != nil {
		c.Error(err)
		return
	}
	if !rows.Next() {
		c.Header("WWW-Authenticate", realm)
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}
	defer rows.Close()
	var dbUser, dbPassword string
	err = rows.Scan(&dbUser, &dbPassword)
	if err != nil {
		c.Error(err)
		return
	}
	err = bcrypt.CompareHashAndPassword([]byte(dbPassword), []byte(password))
	if err != nil {
		c.Header("WWW-Authenticate", realm)
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}
	c.Set(gin.AuthUserKey, user)
}

//go:embed templates/*
var templatesDir embed.FS

//go:embed public
var staticDir embed.FS

func initdb(dbPath string) {
	var err error
	db, err = sql.Open("sqlite3", dbPath)
	if err != nil {
		panic(err)
	}
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS users (
    username VARCHAR(50) NOT NULL PRIMARY KEY,
    password VARCHAR(50) NOT NULL,
    instance_limit INT NOT NULL
)`)
	if err != nil {
		panic(err)
	}
}

func main() {
	port := flag.Int("port", 8080, "port to listen on")
	defaultProfile := flag.String("profile", "default", "default profile to use")
	dbPath := flag.String("db", "lxclab.sqlite3", "path to sqlite3 database")

	flag.Parse()

	var err error
	client, err = NewLXCClient(*defaultProfile)
	if err != nil {
		panic(err)
	}
	initdb(*dbPath)
	r := gin.Default()
	templ := template.Must(template.New("").ParseFS(templatesDir, "templates/*.html"))
	r.SetHTMLTemplate(templ)
	public, err := fs.Sub(staticDir, "public")
	if err != nil {
		panic(err)
	}
	r.StaticFS("/public", http.FS(public))
	r.GET("/", BasicAuth, home)
	r.GET("/shell/:name", BasicAuth, shell)
	r.POST("/create", BasicAuth, create)
	r.POST("/start/:name", BasicAuth, start)
	r.POST("/stop/:name", BasicAuth, stop)
	r.POST("/delete/:name", BasicAuth, delete)
	r.GET("/terminal/:name", BasicAuth, terminal)
	r.GET("/signup", signup)
	r.POST("/signup", signup)
	r.Run(fmt.Sprintf(":%d", *port))
}
