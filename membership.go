package staticbackend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"staticbackend/cache"
	"staticbackend/internal"
	"staticbackend/middleware"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"golang.org/x/crypto/bcrypt"

	"github.com/gbrlsnchs/jwt/v3"
)

type membership struct {
	volatile *cache.Cache
}

func (m *membership) emailExists(w http.ResponseWriter, r *http.Request) {
	email := strings.ToLower(r.URL.Query().Get("e"))
	if len(email) == 0 {
		respond(w, http.StatusOK, false)
		return
	}

	conf, _, err := middleware.Extract(r, false)
	if err != nil {
		http.Error(w, "invalid StaticBackend key", http.StatusUnauthorized)
		return
	}

	db := client.Database(conf.Name)

	ctx, _ := context.WithTimeout(context.Background(), 2*time.Second)
	count, err := db.Collection("sb_tokens").CountDocuments(ctx, bson.M{"email": email})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	respond(w, http.StatusOK, count == 1)
}

func (m *membership) login(w http.ResponseWriter, r *http.Request) {
	conf, _, err := middleware.Extract(r, false)
	if err != nil {
		http.Error(w, "invalid StaticBackend key", http.StatusUnauthorized)
		return
	}

	db := client.Database(conf.Name)

	var l internal.Login
	if err := json.NewDecoder(r.Body).Decode(&l); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	l.Email = strings.ToLower(l.Email)

	tok, err := m.validateUserPassword(db, l.Email, l.Password)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	token := fmt.Sprintf("%s|%s", tok.ID.Hex(), tok.Token)

	// get their JWT
	jwtBytes, err := m.getJWT(token)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	auth := internal.Auth{
		AccountID: tok.AccountID,
		UserID:    tok.ID,
		Email:     tok.Email,
		Role:      tok.Role,
		Token:     tok.Token,
	}

	//TODO: find a good way to find all occurences of those two
	// and make them easily callable via a shared function
	if err := m.volatile.SetTyped(token, auth); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := m.volatile.SetTyped("base:"+token, conf); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	respond(w, http.StatusOK, string(jwtBytes))
}

func (m *membership) validateUserPassword(db *mongo.Database, email, password string) (*internal.Token, error) {
	email = strings.ToLower(email)

	ctx := context.Background()
	sr := db.Collection("sb_tokens").FindOne(ctx, bson.M{"email": email})

	var tok internal.Token
	if err := sr.Decode(&tok); err != nil {
		return nil, err
	}

	if err := bcrypt.CompareHashAndPassword([]byte(tok.Password), []byte(password)); err != nil {
		return nil, errors.New("invalid email/password")
	}

	return &tok, nil
}

func (m *membership) register(w http.ResponseWriter, r *http.Request) {
	conf, _, err := middleware.Extract(r, false)
	if err != nil {
		http.Error(w, "invalid StaticBackend key", http.StatusUnauthorized)
		log.Println("invalid StaticBackend key")
		return
	}

	db := client.Database(conf.Name)

	var l internal.Login
	if err := json.NewDecoder(r.Body).Decode(&l); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	l.Email = strings.ToLower(l.Email)

	// make sure this email does not exists
	count, err := db.Collection("sb_tokens").CountDocuments(context.Background(), bson.M{"email": l.Email})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	} else if count > 0 {
		http.Error(w, "invalid email", http.StatusBadRequest)
		return
	}

	jwtBytes, tok, err := m.createAccountAndUser(db, l.Email, l.Password, 0)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	token := string(jwtBytes)

	auth := internal.Auth{
		AccountID: tok.AccountID,
		UserID:    tok.ID,
		Email:     tok.Email,
		Role:      tok.Role,
		Token:     tok.Token,
	}

	if err := m.volatile.SetTyped(token, auth); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := m.volatile.SetTyped("base:"+token, conf); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	respond(w, http.StatusOK, token)
}

func (m *membership) createAccountAndUser(db *mongo.Database, email, password string, role int) ([]byte, internal.Token, error) {
	acctID := primitive.NewObjectID()

	a := internal.Account{
		ID:    acctID,
		Email: email,
	}

	ctx := context.Background()
	_, err := db.Collection("sb_accounts").InsertOne(ctx, a)
	if err != nil {
		return nil, internal.Token{}, err
	}

	jwtBytes, tok, err := m.createUser(db, acctID, email, password, role)
	if err != nil {
		return nil, internal.Token{}, err
	}
	return jwtBytes, tok, nil
}

func (m *membership) createUser(db *mongo.Database, accountID primitive.ObjectID, email, password string, role int) ([]byte, internal.Token, error) {
	ctx := context.Background()

	b, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, internal.Token{}, err
	}

	tok := internal.Token{
		ID:        primitive.NewObjectID(),
		AccountID: accountID,
		Email:     email,
		Token:     primitive.NewObjectID().Hex(),
		Password:  string(b),
		Role:      role,
	}

	_, err = db.Collection("sb_tokens").InsertOne(ctx, tok)
	if err != nil {
		return nil, tok, err
	}

	token := fmt.Sprintf("%s|%s", tok.ID.Hex(), tok.Token)

	// Get their JWT
	jwtBytes, err := m.getJWT(token)
	if err != nil {
		return nil, tok, err
	}

	auth := internal.Auth{
		AccountID: tok.AccountID,
		UserID:    tok.ID,
		Email:     tok.Email,
		Role:      role,
		Token:     tok.Token,
	}
	if err := m.volatile.SetTyped(token, auth); err != nil {
		return nil, tok, err
	}

	return jwtBytes, tok, nil
}

func (m *membership) setResetCode(w http.ResponseWriter, r *http.Request) {
	email := strings.ToLower(r.URL.Query().Get("e"))
	if len(email) == 0 || strings.Index(email, "@") <= 0 {
		http.Error(w, "invalid email", http.StatusBadRequest)
		return
	}

	code := randStringRunes(10)

	conf, _, err := middleware.Extract(r, false)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}

	curDB := client.Database(conf.Name)

	tok, err := internal.FindTokenByEmail(curDB, email)
	if err != nil {
		http.Error(w, "email not found", http.StatusNotFound)
		return
	}

	if err := internal.SetPasswordResetCode(curDB, tok.ID, code); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	respond(w, http.StatusOK, code)
}

func (m *membership) resetPassword(w http.ResponseWriter, r *http.Request) {
	conf, _, err := middleware.Extract(r, false)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}

	curDB := client.Database(conf.Name)

	var data = new(struct {
		Email    string `json:"email"`
		Code     string `json:"code"`
		Password string `json:"password"`
	})
	if err := parseBody(r.Body, &data); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	data.Email = strings.ToLower(data.Email)

	b, err := bcrypt.GenerateFromPassword([]byte(data.Password), bcrypt.DefaultCost)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if err := internal.ResetPassword(curDB, data.Email, data.Code, string(b)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	respond(w, http.StatusOK, true)
}

func (m *membership) setRole(w http.ResponseWriter, r *http.Request) {
	conf, a, err := middleware.Extract(r, true)
	if err != nil || a.Role < 100 {
		http.Error(w, "insufficient priviledges", http.StatusUnauthorized)
		return
	}

	var data = new(struct {
		Email string `json:"email"`
		Role  int    `json:"role"`
	})
	if err := parseBody(r.Body, &data); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	db := client.Database(conf.Name)

	ctx, _ := context.WithTimeout(context.Background(), 2*time.Second)
	filter := bson.M{"email": data.Email}
	update := bson.M{"$set": bson.M{"role": data.Role}}
	if _, err := db.Collection("sb_tokens").UpdateOne(ctx, filter, update); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	respond(w, http.StatusOK, true)
}

func (m *membership) setPassword(w http.ResponseWriter, r *http.Request) {
	conf, a, err := middleware.Extract(r, true)
	if err != nil || a.Role < 100 {
		http.Error(w, "insufficient priviledges", http.StatusUnauthorized)
		return
	}

	var data = new(struct {
		Email       string `json:"email"`
		OldPassword string `json:"oldPassword"`
		NewPassword string `json:"newPassword"`
	})
	if err := parseBody(r.Body, &data); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	db := client.Database(conf.Name)

	tok, err := m.validateUserPassword(db, data.Email, data.OldPassword)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}

	newpw, err := bcrypt.GenerateFromPassword([]byte(data.NewPassword), bcrypt.DefaultCost)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	ctx := context.Background()
	filter := bson.M{"_id": tok.ID}
	update := bson.M{"$set": bson.M{"pw": string(newpw)}}
	if _, err := db.Collection("sb_tokens").UpdateOne(ctx, filter, update); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	respond(w, http.StatusOK, true)
}

func (m *membership) getJWT(token string) ([]byte, error) {
	now := time.Now()
	pl := internal.JWTPayload{
		Payload: jwt.Payload{
			Issuer:         "StaticBackend",
			ExpirationTime: jwt.NumericDate(now.Add(12 * time.Hour)),
			NotBefore:      jwt.NumericDate(now.Add(30 * time.Minute)),
			IssuedAt:       jwt.NumericDate(now),
			JWTID:          primitive.NewObjectID().Hex(),
		},
		Token: token,
	}

	return jwt.Sign(pl, internal.HashSecret)

}

func (m *membership) sudoGetTokenFromAccountID(w http.ResponseWriter, r *http.Request) {
	conf, _, err := middleware.Extract(r, false)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	db := client.Database(conf.Name)

	id := ""

	_, r.URL.Path = ShiftPath(r.URL.Path)
	id, r.URL.Path = ShiftPath(r.URL.Path)

	oid, err := primitive.ObjectIDFromHex(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	filter := bson.M{internal.FieldAccountID: oid}
	ctx := context.Background()

	opt := options.Find()
	opt.SetLimit(1)
	opt.SetSort(bson.M{internal.FieldID: 1})

	cur, err := db.Collection("sb_tokens").Find(ctx, filter, opt)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer cur.Close(ctx)

	var tok internal.Token
	if cur.Next(ctx) {
		if err := cur.Decode(&tok); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	if len(tok.Token) == 0 {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	token := fmt.Sprintf("%s|%s", tok.ID.Hex(), tok.Token)

	jwtBytes, err := m.getJWT(token)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	auth := internal.Auth{
		AccountID: tok.AccountID,
		UserID:    tok.ID,
		Email:     tok.Email,
		Role:      tok.Role,
		Token:     tok.Token,
	}
	if err := m.volatile.SetTyped(token, auth); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	respond(w, http.StatusOK, string(jwtBytes))
}
