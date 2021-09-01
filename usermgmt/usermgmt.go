/*
 * @Copyright Reserved By Janusec (https://www.janusec.com/).
 * @Author: U2
 * @Date: 2018-07-14 16:20:35
 * @Last Modified: U2, 2018-07-14 16:20:35
 */

package usermgmt

import (
	"errors"
	"net/http"
	"strconv"

	"janusec/data"
	"janusec/models"
	"janusec/utils"

	"github.com/gorilla/sessions" //cookies 库
)

var (
	store = sessions.NewCookieStore([]byte("janusec-app-gateway"))
)

//从cookie中判断当前用户的登录状态
func IsLogIn(w http.ResponseWriter, r *http.Request) (isLogIn bool, userID int64) {
	session, _ := store.Get(r, "sessionid")
	authUserI := session.Values["authuser"]
	if authUserI != nil {
		authUser := authUserI.(models.AuthUser)
		return true, authUser.UserID
	}
	return false, 0
}

// 从 cookies 中获取用户 id
func GetAuthUser(w http.ResponseWriter, r *http.Request) (*models.AuthUser, error) {
	session, _ := store.Get(r, "sessionid")
	authUserI := session.Values["authuser"]
	if authUserI != nil {
		authUser := authUserI.(models.AuthUser)
		return &authUser, nil
	}
	return nil, errors.New("please login")
}

// 用户登录状态机
func Login(w http.ResponseWriter, r *http.Request, param map[string]interface{}, clientIP string) (*models.AuthUser, error) {
	obj := param["object"].(map[string]interface{})
	username := obj["username"].(string)
	password := obj["passwd"].(string)
	appUser := data.DAL.SelectAppUserByName(username)

	tmpHashpwd := data.SHA256Hash(password + appUser.Salt)
	if tmpHashpwd != appUser.HashPwd {
		return nil, errors.New("wrong authentication credentials")
	}
	// check auth code
	if data.PrimarySetting.AuthenticatorEnabled {
		totpItem, err := GetTOTPByUID(username)
		if err != nil {
			// 首次登陆，需要注册MFA
			// Not exist totp item, means it is the First Login, Create totp key for current uid
			totpKey := genKey()
			_, err := data.DAL.InsertTOTPItem(username, totpKey, false)
			if err != nil {
				utils.DebugPrintln("InsertTOTPItem error", err)
			}
			authUser := &models.AuthUser{
				UserID:       appUser.ID,
				Username:     username,
				Logged:       false,
				TOTPKey:      totpKey,
				TOTPVerified: false,
			}
			return authUser, nil
		}
		//非首次登陆，检查MFA
		if !totpItem.TOTPVerified {
			// 二次验证码触发逻辑
			// TOTP Not Verified, redirect to register
			authUser := &models.AuthUser{
				UserID:       appUser.ID,
				Username:     username,
				Logged:       false,
				TOTPKey:      totpItem.TOTPKey,
				TOTPVerified: false,
			}
			return authUser, nil
		}
		//totpItem.TOTPVerified == true
		// Verify TOTP Auth Code
		totpCode := obj["totp_key"].(string)
		totpCodeInt, _ := strconv.ParseUint(totpCode, 10, 32)
		//校验MFA
		verifyOK := VerifyCode(totpItem.TOTPKey, uint32(totpCodeInt))
		if !verifyOK {
			return nil, errors.New("wrong authentication credentials")
		}
	}

	// 校验ok的场景，包含MFA验证ok的场景
	// auth code ok
	authUser := &models.AuthUser{
		UserID:        appUser.ID,
		Username:      username,
		Logged:        true,                 //登录成功
		IsSuperAdmin:  appUser.IsSuperAdmin, //超管状态
		IsCertAdmin:   appUser.IsCertAdmin,  //证书管理员
		IsAppAdmin:    appUser.IsAppAdmin,   //业务管理员Logout
		NeedModifyPWD: appUser.NeedModifyPWD}
	session, _ := store.Get(r, "sessionid")
	// 登录成功，将 authUser 信息保存在 cookies 中
	session.Values["authuser"] = authUser
	session.Options = &sessions.Options{Path: "/janusec-admin/", MaxAge: 86400 * 7}
	// 保存 cookies，也是调用 http.SetCookie 方法：https://github.com/gorilla/sessions/blob/master/store.go#L101
	err := session.Save(r, w)
	if err != nil {
		utils.DebugPrintln("session save error", err)
	}

	// 记录登录日志
	go utils.AuthLog(clientIP, username, "JANUSEC", "/janusec-admin/")
	return authUser, nil
}

//用户登出逻辑，销毁cookies
func Logout(w http.ResponseWriter, r *http.Request) error {
	session, _ := store.Get(r, "sessionid")
	session.Values["authuser"] = nil                                        //1、去掉session保存的结构
	session.Options = &sessions.Options{Path: "/janusec-admin/", MaxAge: 0} //2、去掉cookies
	//3、强制保存cookie，需要Set-cookie下
	err := session.Save(r, w)
	if err != nil {
		utils.DebugPrintln("session save error", err)
	}
	return nil
}

func GetAppUsers(authUser *models.AuthUser) ([]*models.AppUser, error) {
	var appUsers = []*models.AppUser{}
	queryUsers := data.DAL.SelectAppUsers()
	for _, queryUser := range queryUsers {
		appUser := new(models.AppUser)
		appUser.ID = queryUser.ID
		appUser.Username = queryUser.Username
		if queryUser.Email.Valid {
			appUser.Email = queryUser.Email.String
		} else {
			appUser.Email = ""
		}
		appUser.IsSuperAdmin = queryUser.IsSuperAdmin
		appUser.IsCertAdmin = queryUser.IsCertAdmin
		appUser.IsAppAdmin = queryUser.IsAppAdmin
		if authUser.IsSuperAdmin || authUser.UserID == appUser.ID {
			appUsers = append(appUsers, appUser)
		}
	}
	return appUsers, nil

}

func GetAdmin(param map[string]interface{}) (*models.AppUser, error) {
	var userID = int64(param["id"].(float64))
	return GetAppUserByID(userID)
}

func GetAppUserByID(userID int64) (*models.AppUser, error) {
	if userID > 0 {
		appUser := new(models.AppUser)
		appUser.ID = userID
		queryUser := data.DAL.SelectAppUserByID(userID)
		appUser.Username = queryUser.Username
		if queryUser.Email.Valid {
			appUser.Email = queryUser.Email.String
		} else {
			appUser.Email = ""
		}
		appUser.IsSuperAdmin = queryUser.IsSuperAdmin
		appUser.IsCertAdmin = queryUser.IsCertAdmin
		appUser.IsAppAdmin = queryUser.IsAppAdmin
		appUser.NeedModifyPWD = queryUser.NeedModifyPWD
		return appUser, nil
	} else {
		return nil, errors.New("id error")
	}
}

func UpdateUser(w http.ResponseWriter, r *http.Request, param map[string]interface{}, clientIP string, authUser *models.AuthUser) (*models.AppUser, error) {
	var user = param["object"].(map[string]interface{})
	var userID = int64(user["id"].(float64))
	var username = user["username"].(string)
	var password string
	if user["password"] == nil {
		password = ""
	} else {
		password = user["password"].(string)
	}
	email := ""
	if user["email"] != nil {
		email = user["email"].(string)
	}
	isSuperAdmin := false
	isCertAdmin := false
	isAppAdmin := false
	if authUser.IsSuperAdmin {
		isSuperAdmin = user["is_super_admin"].(bool)
		isCertAdmin = user["is_cert_admin"].(bool)
		isAppAdmin = user["is_app_admin"].(bool)
	}
	salt := data.GetRandomSaltString()
	hashpwd := data.SHA256Hash(password + salt)
	appUser := new(models.AppUser)
	if userID == 0 {
		// new user
		newID, err := data.DAL.InsertIfNotExistsAppUser(username, hashpwd, salt, email, isSuperAdmin, isCertAdmin, isAppAdmin, true)
		if err != nil {
			return nil, err
		}
		appUser.ID = newID
		go utils.OperationLog(clientIP, authUser.Username, "Add User", username)
	} else {
		// update existed user
		if len(password) > 0 {
			err := data.DAL.UpdateAppUserWithPwd(username, hashpwd, salt, email, isSuperAdmin, isCertAdmin, isAppAdmin, false, userID)
			if err != nil {
				return nil, err
			}
			session, _ := store.Get(r, "sessionid")
			authUser := session.Values["authuser"].(models.AuthUser)
			authUser.NeedModifyPWD = false
			session.Values["authuser"] = authUser
			session.Options = &sessions.Options{Path: "/janusec-admin/", MaxAge: 86400 * 7}
			err = session.Save(r, w)
			if err != nil {
				utils.DebugPrintln("session save error", err)
			}
		} else {
			err := data.DAL.UpdateAppUserNoPwd(username, email, isSuperAdmin, isCertAdmin, isAppAdmin, userID)
			if err != nil {
				return nil, err
			}
		}
		appUser.ID = userID
		go utils.OperationLog(clientIP, authUser.Username, "Update User", username)
	}
	appUser.Username = username
	appUser.Email = email
	appUser.IsSuperAdmin = isSuperAdmin
	appUser.IsCertAdmin = isCertAdmin
	appUser.IsAppAdmin = isAppAdmin
	return appUser, nil
}

func DeleteUser(userID int64, clientIP string, authUser *models.AuthUser) error {
	if !authUser.IsSuperAdmin && userID != authUser.UserID {
		return errors.New("delete others is not permitted")
	}
	err := data.DAL.DeleteAppUser(userID)
	go utils.OperationLog(clientIP, authUser.Username, "Delete User", strconv.FormatInt(userID, 10))
	return err
}

//从cookie中获取username
func GetLoginUsername(r *http.Request) string {
	session, _ := store.Get(r, "sessionid")
	authUserI := session.Values["authuser"]
	if authUserI != nil {
		authUser := authUserI.(models.AuthUser)
		return authUser.Username
	}
	return ""
}

// VerifyTOTP for janusec-admin
func VerifyTOTP(uid string, code string) error {
	totpItem, _ := GetTOTPByUID(uid)
	totpCodeInt, _ := strconv.ParseUint(code, 10, 32)
	verifyOK := VerifyCode(totpItem.TOTPKey, uint32(totpCodeInt))
	if verifyOK {
		_, err := UpdateTOTPVerified(totpItem.ID)
		if err != nil {
			utils.DebugPrintln("VerifyTOTP error", err)
		}
		//校验成功
		return nil
	}
	return errors.New("verify failed")
}
