from flask import Flask, jsonify, render_template, request

app = Flask(__name__)
notes = []


@app.get("/")
def index():
    return render_template("index.html", notes=notes)


@app.post("/notes")
def add_note():
    text = request.form.get("text", "").strip()
    if text:
        notes.append(text)
    return index()


@app.get("/api/notes")
def list_notes():
    return jsonify(notes)
