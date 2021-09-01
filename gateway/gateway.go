/*
 * @Copyright Reserved By Janusec (https://www.janusec.com/).
 * @Author: U2
 * @Date: 2018-07-14 16:37:57
 * @Last Modified: U2, 2018-07-14 16:37:57
 */

package gateway

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"janusec/backend"
	"janusec/data"
	"janusec/firewall"
	"janusec/models"
	"janusec/usermgmt"
	"janusec/utils"

	"github.com/gorilla/sessions"
	"github.com/patrickmn/go-cache"
	"github.com/yookoala/gofast"
	"golang.org/x/net/http2"
)

var (
	store   = sessions.NewCookieStore([]byte("janusec"))
	incChan = make(chan int, 8)
	decChan = make(chan int, 8)
)

// Counter stat the concurrency requests
func Counter() {
	for {
		select {
		case <-incChan:
			concurrency++
		case <-decChan:
			concurrency--
		}
	}
}

// ReverseHandlerFunc used for reverse handler
func ReverseHandlerFunc(w http.ResponseWriter, r *http.Request) {
	// inc concurrency（统计并发量，非控制）
	incChan <- 1
	defer func() {
		decChan <- 1
	}()
	// r.Host may has the format: domain:port, first remove port
	domainStr := r.Host
	index := strings.IndexByte(r.Host, ':')
	if index > 0 {
		//r.Host = r.Host[0:index]
		domainStr = r.Host[0:index]
	}
	// 获取domainStr对应的配置，domainStr为a.com/b.com/c.com......
	domain := backend.GetDomainByName(domainStr)
	if domain != nil && domain.Redirect {
		//配置了302的直接跳转
		RedirectRequest(w, r, domain.Location)
		return
	}
	// 获取domainStr对应的app配置
	app := backend.GetApplicationByDomain(domainStr)
	if app == nil {
		// Static Web site
		// 如果未寻找到对应的app配置，则返回静态页面
		staticHandler := http.FileServer(http.Dir("./static/welcome"))
		if strings.HasSuffix(r.URL.Path, "/") {
			targetFile := "./static/welcome" + r.URL.Path + "index.html"
			http.ServeFile(w, r, targetFile)
			return
		}
		staticHandler.ServeHTTP(w, r)
		return
	}
	if (r.TLS == nil) && (app.RedirectHTTPS) {
		//如果开启了https跳转，则将请求跳转至https
		if data.CFG.ListenHTTPS == ":443" {
			RedirectRequest(w, r, "https://"+domainStr+r.URL.Path)
		} else {
			RedirectRequest(w, r, "https://"+domainStr+data.CFG.ListenHTTPS+r.URL.Path)
		}
		return
	}

	//设置处理请求转发到后端应用

	//设置 request URI 中的Scheme和HOST
	r.URL.Scheme = app.InternalScheme
	r.URL.Host = r.Host

	nowTimeStamp := time.Now().Unix()
	// dynamic
	srcIP := GetClientIP(r, app)
	ua := r.UserAgent()

	// 处理IP规则
	// IP Policy
	isAllowIP := false
	//如果ClientIPMethod是IPMethod_REMOTE_ADDR类型（直连类型？）
	if app.ClientIPMethod == models.IPMethod_REMOTE_ADDR {
		// First check whether it has IP Policy
		ipPolicy := firewall.GetIPPolicyByIPAddr(srcIP)
		//根据IP查找ip对应的处理类型
		if ipPolicy != nil {
			if ipPolicy.ApplyToCC {
				if ipPolicy.IsAllow {
					// Allow list, legal security testing
					isAllowIP = true
				} else {
					// Block IP 15 minutes
					go firewall.AddIP2NFTables(srcIP, 900.0)
					return
				}
			}
		}
	}

	//1.非IPMethod_REMOTE_ADDR类型
	//2.未寻找到源IP策略

	// 5-second shield from v1.2.0
	if !isAllowIP && app.ShieldEnabled {
		//非白名单IP且开启了ShieldEnabled策略
		session, _ := store.Get(r, "janusec-token")
		// check authorization
		// 从cookies-store中尝试获取shldtoken
		shldToken := session.Values["shldtoken"]
		if shldToken == nil {
			//如果没有shldtoken
			isSearchEngine := false
			//判断是否为爬虫
			if data.NodeSetting.SkipSEEnabled {
				isSearchEngine = IsSearchEngine(ua)
			}
			if !isSearchEngine {
				isCrawler := IsCrawler(r, srcIP)
				if isCrawler {
					// 判断是否为爬虫
					// Block IP
					go firewall.AddIP2NFTables(srcIP, 900.0)
					return
				}
				// not search engine, not crawler, show 5-second shield
				GenerateShieldPage(w, r, r.URL.Path)
				return
			}
			// search engine, or authorization ok, continue
		}
	}

	// Check CC
	// 判断源IP是否触发CC攻击
	if !isAllowIP {
		isCC, ccPolicy, clientID, needLog := firewall.IsCCAttack(r, app, srcIP)
		if isCC {
			//cc攻击状态，下发对应的策略
			targetURL := r.URL.Path
			if len(r.URL.RawQuery) > 0 {
				targetURL += "?" + r.URL.RawQuery
			}
			hitInfo := &models.HitInfo{TypeID: 1,
				PolicyID:  ccPolicy.AppID,
				VulnName:  "CC",
				Action:    ccPolicy.Action,
				ClientID:  clientID,
				TargetURL: targetURL,
				BlockTime: nowTimeStamp}
			//根据配置CC防护策略
			switch ccPolicy.Action {
			case models.Action_Block_100:
				if needLog {
					go firewall.LogCCRequest(r, app.ID, srcIP, ccPolicy)
				}
				if app.ClientIPMethod == models.IPMethod_REMOTE_ADDR {
					go firewall.AddIP2NFTables(srcIP, ccPolicy.BlockSeconds)
				}
				GenerateBlockPage(w, hitInfo)
				return
			case models.Action_BypassAndLog_200:
				if needLog {
					go firewall.LogCCRequest(r, app.ID, srcIP, ccPolicy)
				}
			case models.Action_CAPTCHA_300:
				if needLog {
					go firewall.LogCCRequest(r, app.ID, srcIP, ccPolicy)
				}
				captchaHitInfo.Store(hitInfo.ClientID, hitInfo)
				captchaURL := CaptchaEntrance + "?id=" + hitInfo.ClientID
				http.Redirect(w, r, captchaURL, http.StatusTemporaryRedirect)
				return
			}
		}
	}

	// WAF Check
	if !isAllowIP && app.WAFEnabled {
		//waf防护策略开启
		if isHit, policy := firewall.IsRequestHitPolicy(r, app.ID, srcIP); isHit {
			switch policy.Action {
			case models.Action_Block_100:
				vulnName, _ := firewall.VulnMap.Load(policy.VulnID)
				hitInfo := &models.HitInfo{TypeID: 2, PolicyID: policy.ID, VulnName: vulnName.(string)}
				go firewall.LogGroupHitRequest(r, app.ID, srcIP, policy)
				GenerateBlockPage(w, hitInfo)
				return
			case models.Action_BypassAndLog_200:
				go firewall.LogGroupHitRequest(r, app.ID, srcIP, policy)
			case models.Action_CAPTCHA_300:
				go firewall.LogGroupHitRequest(r, app.ID, srcIP, policy)
				clientID := GenClientID(r, app.ID, srcIP)
				targetURL := r.URL.Path
				if len(r.URL.RawQuery) > 0 {
					targetURL += "?" + r.URL.RawQuery
				}
				hitInfo := &models.HitInfo{TypeID: 2,
					PolicyID: policy.ID, VulnName: "Group Policy Hit",
					Action: policy.Action, ClientID: clientID,
					TargetURL: targetURL, BlockTime: nowTimeStamp}
				captchaHitInfo.Store(clientID, hitInfo)
				captchaURL := CaptchaEntrance + "?id=" + clientID
				http.Redirect(w, r, captchaURL, http.StatusTemporaryRedirect)
				return
			default:
				// models.Action_Pass_400 do nothing
			}
		}
	}

	// Check OAuth
	if app.OAuthRequired && data.NodeSetting.AuthConfig.Enabled {
		//检查oauth策略
		session, _ := store.Get(r, "janusec-token")
		//尝试从cookie中获取userid
		usernameI := session.Values["userid"]
		var url string
		if r.TLS != nil {
			if data.CFG.ListenHTTPS == ":443" {
				url = "https://" + domainStr + r.URL.Path
			} else {
				url = "https://" + domainStr + data.CFG.ListenHTTPS + r.URL.Path
			}
		} else {
			url = r.URL.String()
		}
		//fmt.Println("1000", usernameI, url)
		if usernameI == nil {
			// Exec OAuth2 Authentication
			state := data.SHA256Hash(srcIP + url + ua)
			stateSession := session.Values[state]
			//fmt.Println("1001 state=", state, url)
			if stateSession == nil {
				entranceURL, err := getOAuthEntrance(state)
				if err != nil {
					_, err = w.Write([]byte(err.Error()))
					if err != nil {
						utils.DebugPrintln("w.Write error", err)
					}
					return
				}
				// Save Application URL for CallBack
				oauthState := models.OAuthState{
					CallbackURL: url,
					UserID:      ""}
				usermgmt.OAuthCache.Set(state, oauthState, cache.DefaultExpiration)
				session.Values[state] = state
				session.Options = &sessions.Options{Path: "/", MaxAge: 300}
				err = session.Save(r, w)
				if err != nil {
					utils.DebugPrintln("session.Save error", err)
				}
				//fmt.Println("1002 cache state:", oauthState, url, "307 to:", entranceURL)
				http.Redirect(w, r, entranceURL, http.StatusTemporaryRedirect)
				return
			}
			// Has state in session, get UserID from cache
			state = stateSession.(string)
			oauthStateI, found := usermgmt.OAuthCache.Get(state)
			if !found {
				// Time expired, clear session
				session.Options = &sessions.Options{Path: "/", MaxAge: -1}
				err := session.Save(r, w)
				if err != nil {
					utils.DebugPrintln("session.Save error", err)
				}
				http.Redirect(w, r, url, http.StatusTemporaryRedirect)
				return
			}
			// found == true
			oauthState := oauthStateI.(models.OAuthState)
			if oauthState.UserID == "" {
				session.Values["userid"] = nil
				entranceURL, err := getOAuthEntrance(state)
				if err != nil {
					_, err = w.Write([]byte(err.Error()))
					if err != nil {
						utils.DebugPrintln("w.Write error", err)
					}
					return
				}
				http.Redirect(w, r, entranceURL, http.StatusTemporaryRedirect)
				return
			}
			session.Values["userid"] = oauthState.UserID
			session.Values["access_token"] = oauthState.AccessToken
			session.Options = &sessions.Options{Path: "/", MaxAge: int(app.SessionSeconds)}
			err := session.Save(r, w)
			if err != nil {
				utils.DebugPrintln("session.Save error", err)
			}
			http.Redirect(w, r, oauthState.CallbackURL, http.StatusTemporaryRedirect)
			return
		}
		// Exist username in session, Forward username to destination
		accessToken := session.Values["access_token"].(string)
		//r.Header.Set("Authorization", "Bearer "+accessToken)
		// 0.9.15 change to X-Auth-Token
		r.Header.Set("X-Auth-Token", accessToken)
		r.Header.Set("X-Auth-User", usernameI.(string))
	}

	//选择转发的后端路由
	dest := backend.SelectBackendRoute(app, r, srcIP)
	if dest == nil {
		errInfo := &models.InternalErrorInfo{
			Description: "Internal Servers Offline",
		}
		GenerateInternalErrorResponse(w, errInfo)
		return
	}

	// Modify Origin if client http and backend https
	if (r.TLS == nil) && (app.InternalScheme == "https") {
		origin := r.Header.Get("Origin")
		if len(origin) > 0 {
			r.Header.Set("Origin", "https://"+domainStr)
		}
	}

	// Add access log and statistics
	go utils.AccessLog(domainStr, r.Method, srcIP, r.RequestURI, ua)
	go IncAccessStat(app.ID, r.URL.Path)
	referer := r.Referer()
	if len(referer) > 0 {
		go IncRefererStat(app.ID, referer, srcIP, ua)
	}

	if dest.RouteType == models.StaticRoute {
		// Static Web site
		staticHandler := http.FileServer(http.Dir(dest.BackendRoute))
		if strings.HasSuffix(r.URL.Path, "/") {
			targetFile := dest.BackendRoute + strings.Replace(r.URL.Path, dest.RequestRoute, "", 1) + dest.Destination
			http.ServeFile(w, r, targetFile)
			return
		}
		http.StripPrefix(dest.RequestRoute, staticHandler).ServeHTTP(w, r)
		return
	} else if dest.RouteType == models.FastCGIRoute {
		// FastCGI
		connFactory := gofast.SimpleConnFactory("tcp", dest.Destination)
		urlPath := utils.GetRoutePath(r.URL.Path)
		newPath := r.URL.Path
		if urlPath != "/" {
			newPath = strings.Replace(r.URL.Path, dest.RequestRoute, "/", 1)
		}
		fastCGIHandler := gofast.NewHandler(
			gofast.NewFileEndpoint(dest.BackendRoute+newPath)(gofast.BasicSession),
			gofast.SimpleClientFactory(connFactory),
		)
		fastCGIHandler.ServeHTTP(w, r)
		return
	}

	//设置http.reverseproxy的转发属性
	// var transport http.RoundTripper
	transport := &http.Transport{
		TLSHandshakeTimeout:   30 * time.Second,
		IdleConnTimeout:       30 * time.Second,
		ExpectContinueTimeout: 5 * time.Second,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := net.Dial("tcp", dest.Destination)
			dest.CheckTime = nowTimeStamp
			if err != nil {
				dest.Online = false
				utils.DebugPrintln("DialContext error", err)
				if data.NodeSetting.SMTP.SMTPEnabled {
					sendOfflineNotification(app, dest.Destination)
				}
				errInfo := &models.InternalErrorInfo{
					Description: "Internal Server Offline",
				}
				GenerateInternalErrorResponse(w, errInfo)
			}
			return conn, err
		},
		//TLS拨号属性
		DialTLS: func(network, addr string) (net.Conn, error) {
			conn, err := net.Dial("tcp", dest.Destination)
			dest.CheckTime = nowTimeStamp
			if err != nil {
				dest.Online = false
				dest.CheckTime = nowTimeStamp
				utils.DebugPrintln("DialContext error", err)
				if data.NodeSetting.SMTP.SMTPEnabled {
					sendOfflineNotification(app, dest.Destination)
				}
				errInfo := &models.InternalErrorInfo{
					Description: "Internal Server Offline",
				}
				GenerateInternalErrorResponse(w, errInfo)
				return nil, err
			}
			cfg := &tls.Config{
				ServerName:         domainStr,
				NextProtos:         []string{"h2", "http/1.1"},
				MinVersion:         tls.VersionTLS12,
				InsecureSkipVerify: true,
			}
			tlsConn := tls.Client(conn, cfg)
			if err := tlsConn.Handshake(); err != nil {
				utils.DebugPrintln("tlsConn.Handshake error", err)
			}
			return tlsConn, err //net.Dial("tcp", dest)
		},
	}
	err := http2.ConfigureTransport(transport)
	if err != nil {
		utils.DebugPrintln("http2.ConfigureTransport error", err)
	}

	// Check static cache
	isStatic := firewall.IsStaticResource(r)
	if isStatic {
		// First check Header Range, not cache for range
		rangeValue := r.Header.Get("Range")
		if rangeValue == "" {
			staticRoot := fmt.Sprintf("./static/cdncache/%d", app.ID)
			targetFile := staticRoot + r.URL.Path
			// Check Static Cache
			fi, err := os.Stat(targetFile)
			if err == nil {
				// Found targetFile
				now := time.Now()
				fiStat := fi.Sys().(*syscall.Stat_t)
				// Use ctime fiStat.Ctim.Sec to mark the last check time
				pastSeconds := now.Unix() - int64(fiStat.Ctim.Sec)
				if pastSeconds > 1800 {
					// check update
					backendAddr := fmt.Sprintf("%s://%s%s", app.InternalScheme, dest.Destination, r.RequestURI)
					req, err := http.NewRequest("GET", backendAddr, nil)
					if err != nil {
						utils.DebugPrintln("Check Update NewRequest", err)
					}
					if err == nil {
						req.Header.Set("Host", domainStr)
						modTimeGMT := fi.ModTime().UTC().Format(http.TimeFormat)
						//If-Modified-Since: Sun, 14 Jun 2020 13:54:20 GMT
						req.Header.Set("If-Modified-Since", modTimeGMT)
						client := http.Client{
							Transport: transport,
						}
						resp, err := client.Do(req)
						if err != nil {
							utils.DebugPrintln("Cache update Do", err)
							return
						}
						defer resp.Body.Close()
						if resp.StatusCode == http.StatusOK {
							//fmt.Println("200", backendAddr)
							bodyBuf, _ := ioutil.ReadAll(resp.Body)
							err = ioutil.WriteFile(targetFile, bodyBuf, 0600)
							if err != nil {
								utils.DebugPrintln("CDN WriteFile", targetFile, err)
							}
							lastModified, err := time.Parse(http.TimeFormat, resp.Header.Get("Last-Modified"))
							if err != nil {
								utils.DebugPrintln("CDN Parse Last-Modified", targetFile, err)
							}
							err = os.Chtimes(targetFile, now, lastModified)
							if err != nil {
								utils.DebugPrintln("CDN Chtimes", targetFile, err)
							}
						} else if resp.StatusCode == http.StatusNotModified {
							//fmt.Println("304", backendAddr)
							err := os.Chtimes(targetFile, now, fi.ModTime())
							if err != nil {
								utils.DebugPrintln("Cache update access time", err)
							}
						}
					}
				}
				http.ServeFile(w, r, targetFile)
				return
			}
		}
		// Has Range Header, or resource Not Found, Continue
	}

	// Reverse Proxy
	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			//req.URL.Scheme = app.InternalScheme
			//req.URL.Host = r.Host
		},
		Transport:      transport,       //transport属性
		ModifyResponse: rewriteResponse} //支持修改response
	if utils.Debug {
		dump, err := httputil.DumpRequest(r, true)
		if err != nil {
			utils.DebugPrintln("ReverseHandlerFunc DumpRequest", err)
		}
		fmt.Println(string(dump))
	}
	//设置转发代理请求的头部Host字段
	r.Host = domainStr
	//执行真正的代理请求
	proxy.ServeHTTP(w, r)
}

func getOAuthEntrance(state string) (entranceURL string, err error) {
	switch data.NodeSetting.AuthConfig.Provider {
	case "wxwork":
		entranceURL = fmt.Sprintf("https://open.work.weixin.qq.com/wwopen/sso/qrConnect?appid=%s&agentid=%s&redirect_uri=%s&state=%s",
			data.NodeSetting.AuthConfig.Wxwork.CorpID,
			data.NodeSetting.AuthConfig.Wxwork.AgentID,
			data.NodeSetting.AuthConfig.Wxwork.Callback,
			state)
	case "dingtalk":
		entranceURL = fmt.Sprintf("https://oapi.dingtalk.com/connect/qrconnect?appid=%s&response_type=code&scope=snsapi_login&state=%s&redirect_uri=%s",
			data.NodeSetting.AuthConfig.Dingtalk.AppID,
			state,
			data.NodeSetting.AuthConfig.Dingtalk.Callback)
	case "feishu":
		entranceURL = fmt.Sprintf("https://open.feishu.cn/open-apis/authen/v1/index?redirect_uri=%s&app_id=%s&state=%s",
			data.NodeSetting.AuthConfig.Feishu.Callback,
			data.NodeSetting.AuthConfig.Feishu.AppID,
			state)
	case "lark":
		entranceURL = fmt.Sprintf("https://open.larksuite.com/open-apis/authen/v1/index?redirect_uri=%s&app_id=%s&state=%s",
			data.NodeSetting.AuthConfig.Lark.Callback,
			data.NodeSetting.AuthConfig.Lark.AppID,
			state)
	case "ldap":
		entranceURL = "/ldap/login?state=" + state
	case "cas2":
		entranceURL = fmt.Sprintf("%s/login?renew=true&service=%s?state=%s",
			data.NodeSetting.AuthConfig.CAS2.Entrance, data.NodeSetting.AuthConfig.CAS2.Callback, state)
	case "saml":
		entranceURL = "/saml/login?state=" + state
	default:
		//w.Write([]byte("Designated OAuth not supported, please check config.json ."))
		return "", errors.New("the OAuth provider is not supported, please check settings")
	}
	return entranceURL, nil
}

// RedirectRequest for example: redirect 80 to 443
func RedirectRequest(w http.ResponseWriter, r *http.Request, location string) {
	if len(r.URL.RawQuery) > 0 {
		location += "?" + r.URL.RawQuery
	}
	http.Redirect(w, r, location, http.StatusMovedPermanently)
}

// GenClientID generate unique client id
func GenClientID(r *http.Request, appID int64, srcIP string) string {
	preHashContent := srcIP
	url := r.URL.Path
	preHashContent += url
	ua := r.Header.Get("User-Agent")
	preHashContent += ua
	cookie := r.Header.Get("Cookie")
	preHashContent += cookie
	clientID := data.SHA256Hash(preHashContent)
	return clientID
}

// GetClientIP acquire the client IP address
// 根据app中的设置获取不同类型的源IP
func GetClientIP(r *http.Request, app *models.Application) (clientIP string) {
	switch app.ClientIPMethod {
	case models.IPMethod_REMOTE_ADDR:
		clientIP, _, _ = net.SplitHostPort(r.RemoteAddr)
		return clientIP
	case models.IPMethod_X_FORWARDED_FOR:
		xForwardedFor := r.Header.Get("X-Forwarded-For")
		ips := strings.Split(xForwardedFor, ", ")
		clientIP = ips[len(ips)-1]
	case models.IPMethod_X_REAL_IP:
		clientIP = r.Header.Get("X-Real-IP")
	case models.IPMethod_REAL_IP:
		clientIP = r.Header.Get("Real-IP")
	}
	if len(clientIP) == 0 {
		clientIP, _, _ = net.SplitHostPort(r.RemoteAddr)
	}
	return clientIP
}

// OAuthLogout Clear OAuth Information
func OAuthLogout(w http.ResponseWriter, r *http.Request) {
	session, _ := store.Get(r, "janusec-token")
	session.Options = &sessions.Options{Path: "/", MaxAge: -1}
	err := session.Save(r, w)
	if err != nil {
		utils.DebugPrintln("OAuthLogout session.Save error", err)
	}
	http.Redirect(w, r, "/", http.StatusTemporaryRedirect)
}

// DailyRoutineTasks for clear expired logs
func DailyRoutineTasks() {
	for {
		now := time.Now()
		next := now.Add(time.Hour * 24)
		next = time.Date(next.Year(), next.Month(), next.Day(), 3, 0, 0, 0, next.Location()) // AM 03:00
		t := time.NewTimer(next.Sub(now))
		<-t.C

		if data.IsPrimary {
			expiredTime := next.Unix() - 86400*14
			// Clear expired access statistics
			go data.DAL.ClearExpiredAccessStats(expiredTime)
			// Clear expired referer stats
			go data.DAL.ClearExpiredReferStat(expiredTime)
			// Check expiring certificates
			if data.NodeSetting.SMTP.SMTPEnabled {
				CheckExpiringCertificates()
			}
		}

		// Clear expired logs under ./log/
		globalSettings := data.GetGlobalSettings2()
		expiredDays := "+" + strconv.FormatInt(globalSettings.AccessLogDays, 10)
		cmd := exec.Command("find", "./log/", "-mtime", expiredDays, "-delete")
		err := cmd.Run()
		if err != nil {
			utils.DebugPrintln("Delete old log files Error:", err)
		}
	}
}

// CheckExpiringCertificates and send email notification
func CheckExpiringCertificates() {
	now := time.Now().Unix()
	emails := data.DAL.GetCertAdminEmails()
	mailBody := ""
	for _, cert := range backend.Certs {
		remainDays := (cert.ExpireTime - now) / 86400
		if remainDays <= 31 {
			remainDaysStr := strconv.FormatInt(remainDays, 10)
			mailBody += "Certificate: " + cert.CommonName + " is about to expire within " + remainDaysStr + " days.<br>\r\n"
			utils.DebugPrintln("Warning: Certificate: " + cert.CommonName + " remain days: " + remainDaysStr)
		}
	}
	if len(mailBody) > 0 && len(emails) > 0 {
		go utils.SendEmail(data.NodeSetting.SMTP.SMTPServer,
			data.NodeSetting.SMTP.SMTPPort,
			data.NodeSetting.SMTP.SMTPAccount,
			data.NodeSetting.SMTP.SMTPPassword,
			emails,
			"[JANUSEC] Certificate expire notification",
			mailBody)
	}
}

// sendOfflineNotification ...
func sendOfflineNotification(app *models.Application, dest string) {
	var emails string
	if data.IsPrimary {
		emails = data.DAL.GetAppAdminAndOwnerEmails(app.Owner)
	} else {
		emails = data.NodeSetting.SMTP.AdminEmails
	}
	mailBody := "Backend server: " + dest + " (" + app.Name + ") was offline."
	if len(mailBody) > 0 && len(emails) > 0 {
		go utils.SendEmail(data.NodeSetting.SMTP.SMTPServer,
			data.NodeSetting.SMTP.SMTPPort,
			data.NodeSetting.SMTP.SMTPAccount,
			data.NodeSetting.SMTP.SMTPPassword,
			emails,
			"[JANUSEC] Backend server offline notification",
			mailBody)
	}
}

// Test ...
func Test(w http.ResponseWriter, r *http.Request) {
	w.Write([]byte("Done"))
}

// TestSMTP ...
func TestSMTP(r *http.Request) error {
	var smtpTestReq models.SMTPTestRequest
	err := json.NewDecoder(r.Body).Decode(&smtpTestReq)
	if err != nil {
		utils.DebugPrintln("TestSMTP Decode", err)
	}
	defer r.Body.Close()
	smtpSetting := smtpTestReq.Object
	if len(data.NodeSetting.SMTP.AdminEmails) == 0 {
		data.NodeSetting.SMTP.AdminEmails = data.DAL.GetAppAdminEmails()
	}
	go utils.SendEmail(smtpSetting.SMTPServer,
		smtpSetting.SMTPPort,
		smtpSetting.SMTPAccount,
		smtpSetting.SMTPPassword,
		data.NodeSetting.SMTP.AdminEmails,
		"[JANUSEC] Test SMTP",
		"This is a test email to application administrators.")
	return nil
}
