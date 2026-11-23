package proxyconfig

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

const budgetTable = `"LiteLLM_BudgetTable"`

type budgetRecord struct {
	ID string `json:"budget_id"`
	Max *float64 `json:"max_budget"`
	Soft *float64 `json:"soft_budget"`
	Parallel *int `json:"max_parallel_requests"`
	TPM *int `json:"tpm_limit"`
	RPM *int `json:"rpm_limit"`
	Duration *string `json:"budget_duration"`
	Reset *time.Time `json:"budget_reset_at"`
	Model map[string]any `json:"model_max_budget"`
}

func WithBudgetManagement(next http.Handler,db *sql.DB,keys *KeyRegistry) (http.Handler,error) {
	if db==nil {return next,nil}
	ctx,cancel:=context.WithTimeout(context.Background(),5*time.Second)
	defer cancel()
	if _,err:=db.ExecContext(ctx,`CREATE TABLE IF NOT EXISTS `+budgetTable+` (
		budget_id TEXT PRIMARY KEY,max_budget DOUBLE PRECISION,soft_budget DOUBLE PRECISION,
		max_parallel_requests INTEGER,tpm_limit INTEGER,rpm_limit INTEGER,budget_duration TEXT,budget_reset_at TIMESTAMPTZ,
		model_max_budget JSONB,created_at TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP,updated_at TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`);err!=nil {return nil,err}
	api:=&budgetAPI{db:db,keys:keys}
	mux:=http.NewServeMux()
	mux.HandleFunc("POST /budget/new",api.write)
	mux.HandleFunc("POST /budget/update",api.write)
	mux.HandleFunc("POST /budget/info",api.info)
	mux.HandleFunc("GET /budget/settings",api.infoQuery)
	mux.HandleFunc("GET /budget/list",api.list)
	mux.HandleFunc("POST /budget/delete",api.delete)
	mux.Handle("/",next)
	return mux,nil
}

type budgetAPI struct {db *sql.DB; keys *KeyRegistry}

func (api *budgetAPI) admin(w http.ResponseWriter,r *http.Request) bool {
	return api.keys!=nil && api.keys.requireMaster(w,r)
}

func (api *budgetAPI) write(w http.ResponseWriter,r *http.Request) {
	if !api.admin(w,r) {return}
	var input budgetRecord
	if decodeModelJSON(http.MaxBytesReader(w,r.Body,1<<20),&input)!=nil {
		modelError(w,400,"Invalid budget configuration");return
	}
	if input.ID=="" {input.ID=newEntityID()}
	if r.URL.Path=="/budget/update" {
		var current budgetRecord
		var model []byte
		err:=api.db.QueryRow(`SELECT budget_id,max_budget,soft_budget,max_parallel_requests,tpm_limit,rpm_limit,budget_duration,budget_reset_at,model_max_budget FROM `+budgetTable+` WHERE budget_id=$1`,input.ID).
			Scan(&current.ID,&current.Max,&current.Soft,&current.Parallel,&current.TPM,&current.RPM,&current.Duration,&current.Reset,&model)
		if err!=nil {
			if errors.Is(err,sql.ErrNoRows) {modelError(w,404,"Budget not found")} else {modelError(w,500,"Could not read budget")}
			return
		}
		if input.Max==nil {input.Max=current.Max};if input.Soft==nil {input.Soft=current.Soft};if input.Parallel==nil {input.Parallel=current.Parallel}
		if input.TPM==nil {input.TPM=current.TPM};if input.RPM==nil {input.RPM=current.RPM};if input.Duration==nil {input.Duration=current.Duration};if input.Reset==nil {input.Reset=current.Reset}
		if input.Model==nil {_=json.Unmarshal(model,&input.Model)}
	}
	raw,_:=json.Marshal(input.Model)
	_,err:=api.db.ExecContext(r.Context(),`INSERT INTO `+budgetTable+`
		(budget_id,max_budget,soft_budget,max_parallel_requests,tpm_limit,rpm_limit,budget_duration,budget_reset_at,model_max_budget,updated_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,CURRENT_TIMESTAMP)
		ON CONFLICT (budget_id) DO UPDATE SET max_budget=EXCLUDED.max_budget,soft_budget=EXCLUDED.soft_budget,
		max_parallel_requests=EXCLUDED.max_parallel_requests,tpm_limit=EXCLUDED.tpm_limit,rpm_limit=EXCLUDED.rpm_limit,
		budget_duration=EXCLUDED.budget_duration,budget_reset_at=EXCLUDED.budget_reset_at,model_max_budget=EXCLUDED.model_max_budget,updated_at=CURRENT_TIMESTAMP`,
		input.ID,input.Max,input.Soft,input.Parallel,input.TPM,input.RPM,input.Duration,input.Reset,string(raw))
	if err!=nil {modelError(w,500,"Could not store budget");return}
	api.infoByID(w,input.ID)
}

func (api *budgetAPI) info(w http.ResponseWriter,r *http.Request) {
	if !api.admin(w,r) {return}
	var input struct {Budgets []string `json:"budgets"`}
	if decodeModelJSON(r.Body,&input)!=nil || len(input.Budgets)!=1 {
		modelError(w,400,"Supply exactly one budget ID");return
	}
	api.infoByID(w,input.Budgets[0])
}

func (api *budgetAPI) infoQuery(w http.ResponseWriter,r *http.Request) {
	if !api.admin(w,r) {return}
	api.infoByID(w,r.URL.Query().Get("budget_id"))
}

func (api *budgetAPI) infoByID(w http.ResponseWriter,id string) {
	if strings.TrimSpace(id)=="" {modelError(w,400,"Budget ID is required");return}
	var row budgetRecord
	var model []byte
	err:=api.db.QueryRow(`SELECT budget_id,max_budget,soft_budget,max_parallel_requests,tpm_limit,rpm_limit,budget_duration,budget_reset_at,model_max_budget FROM `+budgetTable+` WHERE budget_id=$1`,id).
		Scan(&row.ID,&row.Max,&row.Soft,&row.Parallel,&row.TPM,&row.RPM,&row.Duration,&row.Reset,&model)
	if errors.Is(err,sql.ErrNoRows) {modelError(w,404,"Budget not found");return}
	if err!=nil {modelError(w,500,"Could not read budget");return}
	_ = json.Unmarshal(model,&row.Model)
	modelJSON(w,200,row)
}

func (api *budgetAPI) list(w http.ResponseWriter,r *http.Request) {
	if !api.admin(w,r) {return}
	rows,err:=api.db.Query(`SELECT budget_id,max_budget,soft_budget,max_parallel_requests,tpm_limit,rpm_limit,budget_duration,budget_reset_at,model_max_budget FROM `+budgetTable+` ORDER BY created_at DESC,budget_id`)
	if err!=nil {modelError(w,500,"Could not list budgets");return}
	defer rows.Close()
	result:=[]budgetRecord{}
	for rows.Next() {
		var row budgetRecord
		var model []byte
		if err:=rows.Scan(&row.ID,&row.Max,&row.Soft,&row.Parallel,&row.TPM,&row.RPM,&row.Duration,&row.Reset,&model);err!=nil {modelError(w,500,"Could not read budget");return}
		_ = json.Unmarshal(model,&row.Model)
		result=append(result,row)
	}
	modelJSON(w,200,map[string]any{"budgets":result,"total_count":len(result)})
}

func (api *budgetAPI) delete(w http.ResponseWriter,r *http.Request) {
	if !api.admin(w,r) {return}
	var input struct {ID string `json:"id"`}
	if decodeModelJSON(r.Body,&input)!=nil || input.ID=="" {modelError(w,400,"Budget ID is required");return}
	result,err:=api.db.Exec(`DELETE FROM `+budgetTable+` WHERE budget_id=$1`,input.ID)
	if err!=nil {modelError(w,500,"Could not delete budget");return}
	count,_:=result.RowsAffected()
	if count==0 {modelError(w,404,"Budget not found");return}
	modelJSON(w,200,map[string]string{"message":"Budget deleted successfully"})
}
