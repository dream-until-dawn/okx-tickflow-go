// 与 okx-position-simulator-go 对接的适配层，是【独立的嵌套模块】。
//
// 它需要 shopspring/decimal 与整个记账内核，而主模块承诺「依赖树只有
// okx-api-v5-go 一个」。若把它们写进主模块的 go.mod，即便使用者只想拉行情、
// 从不碰仓位核算，那两个依赖仍会出现在他们的模块图里，那条承诺就不成立了。
//
// 代价是仓库根目录的 go build ./... 与 go test ./... 不包含本目录，
// 需单独进入执行。replace 只在本模块作为主模块时生效，被别人引用时走 require。
module github.com/dream-until-dawn/okx-tickflow-go/adapter/simbar

go 1.22

replace github.com/dream-until-dawn/okx-tickflow-go => ../..

require (
	github.com/dream-until-dawn/okx-position-simulator-go v0.9.2
	github.com/dream-until-dawn/okx-tickflow-go v0.4.0
	github.com/shopspring/decimal v1.4.0
)
