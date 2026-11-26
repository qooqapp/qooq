// web is the front end of the todo-stack example. It renders the list on the
// server, fetching from the API over the stack's network (API_INTERNAL), and
// hands the browser the API's outside address (API_PUBLIC) for adding items.
const http = require("http");

const internal = process.env.API_INTERNAL || "http://localhost:8080";
const publicBase = process.env.API_PUBLIC || "http://localhost:8080";

const escape = (s) =>
  String(s).replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]);

http
  .createServer(async (req, res) => {
    if (req.url === "/healthz") {
      res.end("ok");
      return;
    }
    let items = [];
    let error = "";
    try {
      items = await (await fetch(`${internal}/api/todos`)).json();
    } catch (e) {
      error = `cannot reach the API at ${internal}: ${e.message}`;
    }
    res.setHeader("Content-Type", "text/html; charset=utf-8");
    res.end(`<!doctype html>
<meta charset="utf-8">
<title>Todo</title>
<h1>Todo</h1>
${error ? `<p style="color:red">${escape(error)}</p>` : ""}
<ul>${items.map((t) => `<li>${escape(t.title)}</li>`).join("")}</ul>
<form id="add"><input name="title" placeholder="New item" required> <button>Add</button></form>
<script>
document.getElementById("add").onsubmit = async (e) => {
  e.preventDefault();
  await fetch(${JSON.stringify(publicBase)} + "/api/todos", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ title: e.target.title.value }),
  });
  location.reload();
};
</script>
`);
  })
  .listen(3000, () => console.log("web listening on :3000"));
