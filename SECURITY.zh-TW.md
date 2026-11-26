# 安全政策

[English](SECURITY.md) | 繁體中文

## 回報漏洞

安全問題請不要開公開的 issue，改用私下管道回報：

- **Email：** qooqapp1010@gmail.com
- 或本 repo *Security* 分頁裡 GitHub 的 **Report a vulnerability** 按鈕

請附上你發現的問題、重現步驟和版本（`qooq --version`、`qooqd -version`）。
收到後會回信確認；修正版發佈之後，這份回報就可以公開。

## 範圍

涵蓋 [qooqapp/qooq](https://github.com/qooqapp/qooq)（qooqd、qooq-watcher、部署 skill、`install.sh`）
與 [qooqapp/qooq-cli](https://github.com/qooqapp/qooq-cli) 的程式碼。

部署一個專案就是在你的機器上執行它的程式碼，所以「部署了不可信的專案」本身不算漏洞，
安全模型見 [ARCHITECTURE](docs/ARCHITECTURE.zh-TW.md#安全模型)。

## 支援的版本

只修在最新的 release。
