package main

import (
	"bytes"
	crand "crypto/rand"
	"crypto/sha512"
	"encoding/hex"
	"flag"
	"fmt"
	"html/template"
	"io"
	"io/ioutil"
	"log"
	"math/rand"
	"net"
	"net/http"
	_ "net/http/pprof"
	"net/url"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/gorilla/sessions"
	"github.com/jmoiron/sqlx"
	"github.com/zenazn/goji"
	"github.com/zenazn/goji/bind"
	"github.com/zenazn/goji/web"
)

var (
	db *sqlx.DB
)

const (
	csrfToken      = "DEADBEEF"
	postsPerPage   = 20
	ISO8601_FORMAT = "2006-01-02T15:04:05-07:00"
	UploadLimit    = 10 * 1024 * 1024 // 10mb

	// CSRF Token error
	StatusUnprocessableEntity = 422
)

type User struct {
	ID          int       `db:"id"`
	AccountName string    `db:"account_name"`
	Passhash    string    `db:"passhash"`
	Authority   int       `db:"authority"`
	DelFlg      int       `db:"del_flg"`
	CreatedAt   time.Time `db:"created_at"`
}

type Post struct {
	ID           int       `db:"id"`
	UserID       int       `db:"user_id"`
	Imgdata      []byte    `db:"imgdata"`
	Body         string    `db:"body"`
	Mime         string    `db:"mime"`
	CreatedAt    time.Time `db:"created_at"`
	CommentCount int
	Comments     []Comment
	User         User
	CSRFToken    string
}

func (p *Post) Render() template.HTML {
	return template.HTML(PrintPost(p))
}

type Comment struct {
	ID        int       `db:"id"`
	PostID    int       `db:"post_id"`
	UserID    int       `db:"user_id"`
	Comment   string    `db:"comment"`
	CreatedAt time.Time `db:"created_at"`
	User      User
}

func writeImage(id int, mime string, data []byte) {
	fn := imagePath(id, mime)
	err := ioutil.WriteFile(fn, data, 0666)
	if err != nil {
		log.Println("failed to write file; path=%q, err=%v", fn, err)
	}
}

func copyImage(id int, src, mime string) {
	dst := imagePath(id, mime)
	if err := os.Chmod(src, 0666); err != nil {
		log.Println("failed to chmod: path=%v, %v", src, err)
	}
	if err := os.Rename(src, dst); err != nil {
		log.Println("failed to rename; src=%q, dst=%q; %v", src, dst, err)
	}
}

func dbInitialize() {
	sqls := []string{
		"DELETE FROM users WHERE id > 1000",
		"DELETE FROM posts WHERE id > 10000",
		"DELETE FROM comments WHERE id > 100000",
		"UPDATE users SET del_flg = 0",
		"UPDATE users SET del_flg = 1 WHERE id % 50 = 0",
	}
	for _, sql := range sqls {
		db.Exec(sql)
	}
	usersReset()
	renderIndexPosts()
}

func tryLogin(accountName, password string) *User {
	u := User{}
	err := db.Get(&u, "SELECT * FROM users WHERE account_name = ? AND del_flg = 0", accountName)
	if err != nil {
		return nil
	}

	if &u != nil && calculatePasshash(u.AccountName, password) == u.Passhash {
		return &u
	} else if &u == nil {
		return nil
	} else {
		return nil
	}
}

func validateUser(accountName, password string) bool {
	if !(regexp.MustCompile("\\A[0-9a-zA-Z_]{3,}\\z").MatchString(accountName) &&
		regexp.MustCompile("\\A[0-9a-zA-Z_]{6,}\\z").MatchString(password)) {
		return false
	}

	return true
}

func digest(src string) string {
	return fmt.Sprintf("%x", sha512.Sum512([]byte(src)))
}

func calculateSalt(accountName string) string {
	return digest(accountName)
}

func calculatePasshash(accountName, password string) string {
	return digest(password + ":" + calculateSalt(accountName))
}

func getSession(r *http.Request) *Session {
	return sessionStore.Get(r)
}

func getSessionUser(r *http.Request) User {
	session := getSession(r)
	if session.UserId == 0 {
		return User{}
	}
	session.User = userGet(session.UserId)
	return session.User
}

func getFlash(w http.ResponseWriter, r *http.Request, key string) string {
	session := getSession(r)
	value := session.Notice
	if value != "" {
		session.Notice = ""
		sessionStore.Set(w, session)
	}
	return value
}

var (
	commentM     sync.Mutex
	commentStore map[int][]Comment = make(map[int][]Comment)
)

func getCommentsLocked(postID int) []Comment {
	if cs, ok := commentStore[postID]; ok {
		return cs
	}

	var cs []Comment
	query := ("SELECT comments.id, comments.comment, comments.created_at, users.id, users.account_name " +
		" FROM `comments` INNER JOIN users ON comments.user_id = users.id " +
		" WHERE `post_id` = ? ORDER BY comments.`created_at`")

	rows, err := db.Query(query, postID)
	if err != nil {
		log.Println(err)
		return cs
	}
	for rows.Next() {
		var c Comment
		err := rows.Scan(&c.ID, &c.Comment, &c.CreatedAt, &c.User.ID, &c.User.AccountName)
		if err != nil {
			log.Println(err)
			continue
		}
		cs = append(cs, c)
	}
	rows.Close()

	commentStore[postID] = cs
	return cs
}

func getComments(postID int) []Comment {
	commentM.Lock()
	defer commentM.Unlock()
	return getCommentsLocked(postID)
}

func appendComent(c Comment) {
	commentM.Lock()
	cs := getCommentsLocked(c.PostID)
	commentStore[c.PostID] = append(cs, c)
	commentM.Unlock()
}

func makePosts(results []Post, CSRFToken string, allComments bool) ([]Post, error) {
	var posts []Post

	for _, p := range results {
		comments := getComments(p.ID)
		if !allComments && len(comments) > 3 {
			comments = comments[len(comments)-3:]
		}
		p.Comments = comments
		p.CSRFToken = CSRFToken

		p.User = userGet(p.UserID)
		if p.User.DelFlg == 1 {
			continue
		}

		posts = append(posts, p)
		if len(posts) >= postsPerPage {
			break
		}
	}
	return posts, nil
}

func imageURL(p *Post) string {
	ext := ""
	if p.Mime == "image/jpeg" {
		ext = ".jpg"
	} else if p.Mime == "image/png" {
		ext = ".png"
	} else if p.Mime == "image/gif" {
		ext = ".gif"
	}

	return "/image/" + strconv.Itoa(p.ID) + ext
}

func imagePath(id int, mime string) string {
	var ext string
	switch mime {
	case "image/jpeg":
		ext = ".jpg"
	case "image/png":
		ext = ".png"
	case "image/gif":
		ext = ".gif"
	}
	return fmt.Sprintf("../public/image/%d%s", id, ext)
}

func isLogin(u User) bool {
	return u.ID != 0
}

func getCSRFToken(r *http.Request) string {
	session := getSession(r)
	return session.CsrfToken
}

func secureRandomStr(b int) string {
	k := make([]byte, b)
	if _, err := io.ReadFull(crand.Reader, k); err != nil {
		panic("error reading from random source: " + err.Error())
	}
	return hex.EncodeToString(k)
}

func getTemplPath(filename string) string {
	return path.Join("templates", filename)
}

func getInitialize(w http.ResponseWriter, r *http.Request) {
	dbInitialize()
	w.WriteHeader(http.StatusOK)
}

func getLogin(w http.ResponseWriter, r *http.Request) {
	me := getSessionUser(r)
	if isLogin(me) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	sess := getSession(r)
	if sess.Notice == "" {
		w.Write(loginHTML)
		return
	}

	loginTemplate.Execute(w, map[string]interface{}{
		"Me":    me,
		"Flash": sess.Notice,
	})
}

func postLogin(w http.ResponseWriter, r *http.Request) {
	if isLogin(getSessionUser(r)) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	u := tryLogin(r.FormValue("account_name"), r.FormValue("password"))
	session := getSession(r)
	if u != nil {
		session.UserId = u.ID
		session.CsrfToken = csrfToken
		session.Save(r, w)
		http.Redirect(w, r, "/", http.StatusFound)
	} else {
		session.Notice = "アカウント名かパスワードが間違っています"
		session.Save(r, w)
		http.Redirect(w, r, "/login", http.StatusFound)
	}
}

func getRegister(w http.ResponseWriter, r *http.Request) {
	if isLogin(getSessionUser(r)) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	template.Must(template.ParseFiles(
		getTemplPath("layout.html"),
		getTemplPath("register.html")),
	).Execute(w, struct {
		Me    User
		Flash string
	}{User{}, getFlash(w, r, "notice")})
}

func postRegister(w http.ResponseWriter, r *http.Request) {
	if isLogin(getSessionUser(r)) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	accountName, password := r.FormValue("account_name"), r.FormValue("password")

	validated := validateUser(accountName, password)
	if !validated {
		session := getSession(r)
		session.Notice = "アカウント名は3文字以上、パスワードは6文字以上である必要があります"
		session.Save(r, w)
		http.Redirect(w, r, "/register", http.StatusFound)
		return
	}

	exists := 0
	// ユーザーが存在しない場合はエラーになるのでエラーチェックはしない
	db.Get(&exists, "SELECT 1 FROM users WHERE `account_name` = ?", accountName)

	if exists == 1 {
		session := getSession(r)
		session.Notice = "アカウント名がすでに使われています"
		session.Save(r, w)
		http.Redirect(w, r, "/register", http.StatusFound)
		return
	}

	query := "INSERT INTO `users` (`account_name`, `passhash`) VALUES (?,?)"
	passhash := calculatePasshash(accountName, password)
	result, eerr := db.Exec(query, accountName, passhash)
	if eerr != nil {
		fmt.Println(eerr.Error())
		return
	}

	session := getSession(r)
	uid, lerr := result.LastInsertId()
	if lerr != nil {
		fmt.Println(lerr.Error())
		return
	}
	session.UserId = int(uid)
	session.CsrfToken = csrfToken
	session.Save(r, w)
	userAdd(User{ID: int(uid), AccountName: accountName, CreatedAt: time.Now(), Passhash: passhash})
	http.Redirect(w, r, "/", http.StatusFound)
}

func getLogout(w http.ResponseWriter, r *http.Request) {
	session := getSession(r)
	session.UserId = 0
	session.User = User{}
	session.Save(r, w)
	http.Redirect(w, r, "/", http.StatusFound)
}

var (
	indexTemplate       *template.Template
	postsTemplate       *template.Template
	accountNameTempalte *template.Template
	loginTemplate       *template.Template
	loginHTML           []byte
	postIDTemplate      *template.Template

	indexPostsM         sync.Mutex
	indexPostsT         time.Time
	indexPostsRenderedM sync.RWMutex
	indexPostsRendered  template.HTML
)

func init() {
	fmap := template.FuncMap{
		"imageURL": imageURL,
	}

	indexTemplate = template.Must(template.New("layout.html").Funcs(fmap).ParseFiles(
		getTemplPath("layout.html"),
		getTemplPath("index.html"),
	))

	postsTemplate = template.Must(template.New("posts.html").Funcs(fmap).ParseFiles(
		getTemplPath("posts.html"),
		getTemplPath("post.html"),
	))

	accountNameTempalte = template.Must(template.New("layout.html").Funcs(fmap).ParseFiles(
		getTemplPath("layout.html"),
		getTemplPath("user.html"),
		getTemplPath("posts.html"),
		getTemplPath("post.html"),
	))

	loginTemplate = template.Must(template.ParseFiles(
		getTemplPath("layout.html"),
		getTemplPath("login.html")),
	)
	me := User{}
	var buf bytes.Buffer
	loginTemplate.Execute(&buf, struct {
		Me    User
		Flash string
	}{me, ""})
	loginHTML = buf.Bytes()

	postIDTemplate = template.Must(template.New("layout.html").Funcs(fmap).ParseFiles(
		getTemplPath("layout.html"),
		getTemplPath("post_id.html"),
		getTemplPath("post.html")))
}

func renderIndexPosts() {
	now := time.Now()
	indexPostsM.Lock()
	defer indexPostsM.Unlock()
	if indexPostsT.After(now) {
		return
	}
	now = time.Now()

	results := []Post{}
	err := db.Select(&results, "SELECT posts.`id`, `user_id`, `body`, `mime`, posts.`created_at` FROM `posts` ORDER BY `created_at` DESC LIMIT 40")
	if err != nil {
		log.Println(err)
		return
	}

	posts, merr := makePosts(results, csrfToken, false)
	if merr != nil {
		log.Println(merr)
		return
	}

	var b bytes.Buffer
	if err := postsTemplate.Execute(&b, posts); err != nil {
		log.Println(err)
		return
	}

	indexPostsT = now
	indexPostsRenderedM.Lock()
	indexPostsRendered = template.HTML(b.String())
	indexPostsRenderedM.Unlock()
}

func getIndexPosts() template.HTML {
	indexPostsRenderedM.RLock()
	t := indexPostsRendered
	indexPostsRenderedM.RUnlock()
	return t
}

func getIndex(w http.ResponseWriter, r *http.Request) {
	sess := getSession(r)
	me := sess.User
	if me.AccountName == "" && sess.UserId != 0 {
		me = userGet(sess.UserId)
		sess.User = me
	}
	posts := getIndexPosts()
	indexTemplate.Execute(w,
		map[string]interface{}{
			"Me":        me,
			"CSRFToken": csrfToken,
			"Flash":     getFlash(w, r, "notice"),
			"Posts":     posts},
	)
}

func getAccountName(c web.C, w http.ResponseWriter, r *http.Request) {
	user := User{}
	uerr := db.Get(&user, "SELECT * FROM `users` WHERE `account_name` = ? AND `del_flg` = 0", c.URLParams["accountName"])

	if uerr != nil {
		fmt.Println(uerr)
		return
	}

	if user.ID == 0 {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	results := []Post{}
	rerr := db.Select(&results, "SELECT `id`, `user_id`, `body`, `mime`, `created_at` FROM `posts` WHERE `user_id` = ? ORDER BY `created_at` DESC", user.ID)
	if rerr != nil {
		fmt.Println(rerr)
		return
	}
	for i := 0; i < len(results); i++ {
		results[i].User = user
	}

	posts, merr := makePosts(results, getCSRFToken(r), false)
	if merr != nil {
		fmt.Println(merr)
		return
	}

	commentCount := 0
	cerr := db.Get(&commentCount, "SELECT COUNT(*) AS count FROM `comments` WHERE `user_id` = ?", user.ID)
	if cerr != nil {
		fmt.Println(cerr)
		return
	}

	postIDs := []int{}
	for _, r := range results {
		postIDs = append(postIDs, r.ID)
	}
	postCount := len(results)

	commentedCount := 0
	if postCount > 0 {
		s := []string{}
		for range postIDs {
			s = append(s, "?")
		}
		placeholder := strings.Join(s, ", ")

		// convert []int -> []interface{}
		args := make([]interface{}, len(postIDs))
		for i, v := range postIDs {
			args[i] = v
		}

		ccerr := db.Get(&commentedCount, "SELECT COUNT(*) AS count FROM `comments` WHERE `post_id` IN ("+placeholder+")", args...)
		if ccerr != nil {
			fmt.Println(ccerr)
			return
		}
	}

	me := getSessionUser(r)

	accountNameTempalte.Execute(w, struct {
		Posts          []Post
		User           User
		PostCount      int
		CommentCount   int
		CommentedCount int
		Me             User
	}{posts, user, postCount, commentCount, commentedCount, me})
}

func getPosts(w http.ResponseWriter, r *http.Request) {
	m, parseErr := url.ParseQuery(r.URL.RawQuery)
	if parseErr != nil {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Println(parseErr)
		return
	}
	maxCreatedAt := m.Get("max_created_at")
	if maxCreatedAt == "" {
		return
	}

	t, terr := time.Parse(ISO8601_FORMAT, maxCreatedAt)
	if terr != nil {
		fmt.Println(terr)
		return
	}

	results := []Post{}
	err := db.Select(&results, "SELECT posts.`id`, `user_id`, `body`, `mime`, posts.`created_at` FROM `posts` WHERE posts.`created_at` <= ? ORDER BY `created_at` DESC LIMIT 40", t)
	if err != nil {
		fmt.Println(err)
		return
	}

	posts, merr := makePosts(results, csrfToken, false)
	if merr != nil {
		fmt.Println(merr)
		return
	}

	if len(posts) == 0 {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	fmap := template.FuncMap{
		"imageURL": imageURL,
	}

	template.Must(template.New("posts.html").Funcs(fmap).ParseFiles(
		getTemplPath("posts.html"),
	)).Execute(w, posts)
}

func getPostsID(c web.C, w http.ResponseWriter, r *http.Request) {
	pid, err := strconv.Atoi(c.URLParams["id"])
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	results := []Post{}
	rerr := db.Select(&results, "SELECT * FROM `posts` WHERE `id` = ?", pid)
	if rerr != nil {
		fmt.Println(rerr)
		return
	}

	posts, merr := makePosts(results, getCSRFToken(r), true)
	if merr != nil {
		log.Println(merr)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if len(posts) == 0 {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	p := posts[0]
	me := getSessionUser(r)
	postIDTemplate.Execute(w, struct {
		Post *Post
		Me   User
	}{&p, me})
}

var uploadM sync.Mutex

func postIndex(w http.ResponseWriter, r *http.Request) {
	me := getSessionUser(r)
	if !isLogin(me) {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	uploadM.Lock()
	defer uploadM.Unlock()
	r.ParseMultipartForm(1 << 10)
	if r.FormValue("csrf_token") != getCSRFToken(r) {
		w.WriteHeader(StatusUnprocessableEntity)
		return
	}

	file, header, ferr := r.FormFile("file")
	if ferr != nil {
		session := getSession(r)
		session.Notice = "画像が必須です"
		session.Save(r, w)
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	mime := ""
	if file != nil {
		defer file.Close()
		// 投稿のContent-Typeからファイルのタイプを決定する
		contentType := header.Header["Content-Type"][0]
		if strings.Contains(contentType, "jpeg") {
			mime = "image/jpeg"
		} else if strings.Contains(contentType, "png") {
			mime = "image/png"
		} else if strings.Contains(contentType, "gif") {
			mime = "image/gif"
		} else {
			session := getSession(r)
			session.Notice = "投稿できる画像形式はjpgとpngとgifだけです"
			session.Save(r, w)
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
	}

	tf, err := ioutil.TempFile("../upload", "img-")
	if err != nil {
		log.Panicf("failed to create image: %v", err)
	}
	written, err := io.CopyN(tf, file, UploadLimit+1)
	if err != nil && err != io.EOF {
		log.Panicf("failed to write to temporary file: %v", err)
	}
	if written > UploadLimit {
		os.Remove(tf.Name())
		tf.Close()
		session := getSession(r)
		session.Notice = "ファイルサイズが大きすぎます"
		session.Save(r, w)
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	query := "INSERT INTO `posts` (`user_id`, `mime`, `imgdata`, `body`) VALUES (?,?,?,?)"
	result, eerr := db.Exec(
		query,
		me.ID,
		mime,
		[]byte(""),
		r.FormValue("body"),
	)
	if eerr != nil {
		fmt.Println(eerr.Error())
		return
	}

	pid, lerr := result.LastInsertId()
	if lerr != nil {
		fmt.Println(lerr.Error())
		return
	}
	tf.Close()
	copyImage(int(pid), tf.Name(), mime)

	time.Sleep(time.Millisecond * 200)
	renderIndexPosts()
	http.Redirect(w, r, "/posts/"+strconv.FormatInt(pid, 10), http.StatusFound)
}

func getImage(c web.C, w http.ResponseWriter, r *http.Request) {
	pidStr := c.URLParams["id"]
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	post := Post{}
	derr := db.Get(&post, "SELECT * FROM `posts` WHERE `id` = ?", pid)
	if derr != nil {
		fmt.Println(derr.Error())
		return
	}

	ext := c.URLParams["ext"]

	if ext == "jpg" && post.Mime == "image/jpeg" ||
		ext == "png" && post.Mime == "image/png" ||
		ext == "gif" && post.Mime == "image/gif" {
		w.Header().Set("Content-Type", post.Mime)
		_, err := w.Write(post.Imgdata)
		if err != nil {
			fmt.Println(err.Error())
		}
		writeImage(pid, post.Mime, post.Imgdata)
		return
	}

	w.WriteHeader(http.StatusNotFound)
}

func postComment(w http.ResponseWriter, r *http.Request) {
	me := getSessionUser(r)
	if !isLogin(me) {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	if r.FormValue("csrf_token") != getCSRFToken(r) {
		w.WriteHeader(StatusUnprocessableEntity)
		return
	}

	postID, ierr := strconv.Atoi(r.FormValue("post_id"))
	if ierr != nil {
		fmt.Println("post_idは整数のみです")
		return
	}

	now := time.Now()
	commentStr := r.FormValue("comment")
	query := "INSERT INTO `comments` (`post_id`, `user_id`, `comment`, `created_at`) VALUES (?,?,?,?)"
	res, err := db.Exec(query, postID, me.ID, commentStr, now)
	if err != nil {
		log.Println(err)
		return
	}
	lid, _ := res.LastInsertId()
	c := Comment{
		ID:        int(lid),
		PostID:    postID,
		UserID:    me.ID,
		Comment:   commentStr,
		CreatedAt: now,
		User:      me,
	}
	appendComent(c)
	time.Sleep(time.Millisecond * 200)
	renderIndexPosts()
	http.Redirect(w, r, fmt.Sprintf("/posts/%d", postID), http.StatusFound)
}

func getAdminBanned(w http.ResponseWriter, r *http.Request) {
	me := getSessionUser(r)
	if !isLogin(me) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	if me.Authority == 0 {
		w.WriteHeader(http.StatusForbidden)
		return
	}

	users := []User{}
	err := db.Select(&users, "SELECT * FROM `users` WHERE `authority` = 0 AND `del_flg` = 0 ORDER BY `created_at` DESC")
	if err != nil {
		fmt.Println(err)
		return
	}

	template.Must(template.ParseFiles(
		getTemplPath("layout.html"),
		getTemplPath("banned.html")),
	).Execute(w, struct {
		Users     []User
		Me        User
		CSRFToken string
	}{users, me, getCSRFToken(r)})
}

func postAdminBanned(w http.ResponseWriter, r *http.Request) {
	me := getSessionUser(r)
	if !isLogin(me) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	if me.Authority == 0 {
		w.WriteHeader(http.StatusForbidden)
		return
	}

	if r.FormValue("csrf_token") != getCSRFToken(r) {
		w.WriteHeader(StatusUnprocessableEntity)
		return
	}

	query := "UPDATE `users` SET `del_flg` = ? WHERE `id` = ?"

	r.ParseForm()
	for _, id := range r.Form["uid[]"] {
		db.Exec(query, 1, id)
		iid, err := strconv.Atoi(id)
		if err != nil {
			userBan(iid, 1)
		}
	}

	time.Sleep(time.Millisecond * 200)
	renderIndexPosts()
	http.Redirect(w, r, "/admin/banned", http.StatusFound)
}

func main() {
	host := os.Getenv("ISUCONP_DB_HOST")
	if host == "" {
		host = "localhost"
	}
	port := os.Getenv("ISUCONP_DB_PORT")
	if port == "" {
		port = "3306"
	}
	_, err := strconv.Atoi(port)
	if err != nil {
		log.Fatalf("Failed to read DB port number from an environment variable ISUCONP_DB_PORT.\nError: %s", err.Error())
	}
	user := os.Getenv("ISUCONP_DB_USER")
	if user == "" {
		user = "root"
	}
	password := os.Getenv("ISUCONP_DB_PASSWORD")
	dbname := os.Getenv("ISUCONP_DB_NAME")
	if dbname == "" {
		dbname = "isuconp"
	}

	dsn := fmt.Sprintf(
		"%s:%s@tcp(%s:%s)/%s?charset=utf8mb4&parseTime=true&loc=Local&interpolateParams=true",
		user,
		password,
		host,
		port,
		dbname,
	)

	db, err = sqlx.Open("mysql", dsn)
	if err != nil {
		log.Fatalf("Failed to connect to DB: %s.", err.Error())
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(8)
	defer db.Close()

	for {
		if db.Ping() == nil {
			break
		}
		log.Println("waiting db...")
	}

	usersReset()
	renderIndexPosts()

	go http.ListenAndServe(":3000", nil)

	goji.DefaultMux = web.New()
	goji.Get("/initialize", getInitialize)
	goji.Get("/login", getLogin)
	goji.Post("/login", postLogin)
	goji.Get("/register", getRegister)
	goji.Post("/register", postRegister)
	goji.Get("/logout", getLogout)
	goji.Get("/", getIndex)
	goji.Get(regexp.MustCompile(`^/@(?P<accountName>[a-zA-Z]+)$`), getAccountName)
	goji.Get("/posts", getPosts)
	goji.Get("/posts/:id", getPostsID)
	goji.Post("/", postIndex)
	goji.Get("/image/:id.:ext", getImage)
	goji.Post("/comment", postComment)
	goji.Get("/admin/banned", getAdminBanned)
	goji.Post("/admin/banned", postAdminBanned)
	goji.Get("/*", http.FileServer(http.Dir("../public")))

	if !flag.Parsed() {
		flag.Parse()
	}
	listener := bind.Default()
	if ul, ok := listener.(*net.UnixListener); ok {
		os.Chmod(ul.Addr().String(), 0777)
	}
	goji.ServeListener(listener)
}

const sessionName = "isucon_session"

type Session struct {
	UserId    int
	User      User
	Key       string
	Notice    string
	CsrfToken string
}

func (s *Session) Save(r *http.Request, w http.ResponseWriter) {
	sessionStore.Set(w, s)
}

type SessionStore struct {
	sync.Mutex
	store map[string]*Session
}

var sessionStore = SessionStore{
	store: make(map[string]*Session),
}

func (self *SessionStore) Get(r *http.Request) *Session {
	cookie, _ := r.Cookie(sessionName)
	if cookie == nil {
		return &Session{}
	}
	key := cookie.Value
	self.Lock()
	s := self.store[key]
	self.Unlock()
	if s == nil {
		s = &Session{}
	}
	return s
}

func (self *SessionStore) Set(w http.ResponseWriter, sess *Session) {
	key := sess.Key
	if key == "" {
		b := make([]byte, 8)
		rand.Read(b)
		key = hex.EncodeToString(b)
		sess.Key = key
	}

	cookie := sessions.NewCookie(sessionName, key, &sessions.Options{})
	http.SetCookie(w, cookie)

	self.Lock()
	self.store[key] = sess
	self.Unlock()
}

var (
	userRepoM sync.Mutex
	userRepo  map[int]User
)

func userAdd(u User) {
	userRepoM.Lock()
	userRepo[u.ID] = u
	userRepoM.Unlock()
}

func userGet(uid int) User {
	userRepoM.Lock()
	u := userRepo[uid]
	userRepoM.Unlock()
	return u
}

func userBan(uid, ban int) {
	userRepoM.Lock()
	u := userRepo[uid]
	u.DelFlg = ban
	userRepo[uid] = u
	userRepoM.Unlock()
}

func usersReset() {
	userRepoM.Lock()
	defer userRepoM.Unlock()

	userRepo = make(map[int]User)
	users := []User{}
	err := db.Select(&users, "SELECT * FROM users")
	if err != nil {
		panic(err)
	}
	for _, u := range users {
		userRepo[u.ID] = u
	}
}
