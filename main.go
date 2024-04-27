package main

import (
	"embed"
	"encoding/json"
	"html/template"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

var client *LXCClient

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
	defer conn.Close()
	stdinReader, stdinWriter := io.Pipe()
	stdoutReader, stdoutWriter := io.Pipe()
	ch := make(chan [2]int)
	go func() {
		for {
			messageType, message, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if messageType == websocket.TextMessage {
				stdinWriter.Write(message)
			} else {
				var windowSize struct {
					High  int `json:"high"`
					Width int `json:"width"`
				}
				if err := json.Unmarshal(message, &windowSize); err != nil {
					continue
				}
				ch <- [2]int{windowSize.Width, windowSize.High}
			}
		}
	}()
	go func() {
		for {
			data := make([]byte, 10240)
			n, err := stdoutReader.Read(data)
			if err != nil {
				continue
			}
			if n > 0 {
				conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
				conn.WriteMessage(websocket.BinaryMessage, data[:n])
			}
		}
	}()
	dataDone := make(chan bool)
	err = client.StartShell(name, stdinReader, stdoutWriter, ch, dataDone)
	if err != nil {
		c.Error(err)
		return
	}
	<-dataDone
}

//go:embed templates/*
var f embed.FS

func main() {
	port := os.Args[1]
	defaultProfile := os.Args[2]

	var err error
	client, err = NewLXCClient(defaultProfile)
	if err != nil {
		panic(err)
	}
	r := gin.Default()
	r.Use(gin.BasicAuth(gin.Accounts{
		"admin":  "admin",
		"admin1": "admin1",
		"admin2": "admin2",
	}))
	templ := template.Must(template.New("").ParseFS(f, "templates/*.html"))
	r.SetHTMLTemplate(templ)
	r.GET("/", home)
	r.GET("/shell/:name", shell)
	r.POST("/create", create)
	r.POST("/start/:name", start)
	r.POST("/stop/:name", stop)
	r.POST("/delete/:name", delete)
	r.GET("/terminal/:name", terminal)
	r.Run(":" + port)
}
