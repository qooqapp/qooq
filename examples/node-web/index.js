// node-web — minimal Express example for qooq's built-in `node` build
// template (qooq.yaml: build.template: node). No Dockerfile needed.
const express = require("express");

const app = express();

app.get("/hello", (req, res) => {
  res.json({ message: "hello from node-web" });
});

app.listen(3000, () => {
  console.log("node-web listening on :3000");
});
