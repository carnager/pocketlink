package io.github.carnager.pocketlink

import android.Manifest
import android.content.Context
import android.content.pm.PackageManager
import android.media.MediaScannerConnection
import android.os.FileObserver
import android.os.Build
import android.os.Environment
import android.os.StatFs
import android.os.SystemClock
import android.util.Log
import java.io.File
import java.io.IOException
import java.io.RandomAccessFile
import java.nio.ByteBuffer
import java.nio.channels.FileChannel
import java.nio.file.AccessDeniedException
import java.nio.file.DirectoryNotEmptyException
import java.nio.file.FileAlreadyExistsException
import java.nio.file.FileSystemException
import java.nio.file.FileVisitResult
import java.nio.file.Files
import java.nio.file.LinkOption
import java.nio.file.NoSuchFileException
import java.nio.file.NotDirectoryException
import java.nio.file.OpenOption
import java.nio.file.Path
import java.nio.file.SimpleFileVisitor
import java.nio.file.StandardCopyOption
import java.nio.file.StandardOpenOption
import java.nio.file.attribute.BasicFileAttributes
import java.nio.file.attribute.FileTime
import java.util.concurrent.ConcurrentHashMap
import java.util.concurrent.Executors
import java.util.concurrent.ScheduledFuture
import java.util.concurrent.TimeUnit
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.RequestBody
import okhttp3.WebSocket
import okio.BufferedSink
import org.json.JSONArray
import org.json.JSONException
import org.json.JSONObject

private const val TAG = "pocketlink"

/**
 * Answers the desktop's fs.* requests from shared storage, so a computer can
 * browse and change the phone's files. See internal/proto/fs.go in the
 * desktop code for the protocol; internal/phonefs/server.go is the
 * reference this mirrors.
 *
 * Requests are stateless (no open handles), so a dropped connection leaves
 * nothing behind.
 */
object PhoneFs {
    /** Largest payload of one frame, as proto.MaxPayload. */
    const val MAX_PAYLOAD = 512 * 1024

    // Enough to overlap a few chunk requests without letting a burst of
    // them hog the phone.
    private val pool = Executors.newFixedThreadPool(4) { Thread(it, "pocketlink-fs") }
    // Streams last as long as a transfer, so they don't take turns in the pool.
    private val streams = Executors.newCachedThreadPool { Thread(it, "pocketlink-fs-stream") }
    private val scanner = Executors.newSingleThreadScheduledExecutor { Thread(it, "pocketlink-scan") }
    private val toScan = LinkedHashSet<String>() // guarded by itself
    private var scanJob: ScheduledFuture<*>? = null

    // Paths the computer changed just now, by when. Watches don't report
    // these back: the computer knows, and telling it would only make it
    // throw away a folder listing it just updated.
    private val own = ConcurrentHashMap<String, Long>()
    private const val OWN_MS = 5000L
    private val CHANGES = setOf("fs.write", "fs.create", "fs.mkdir", "fs.remove", "fs.rename", "fs.setattr", "fs.writestream")

    // OkHttp closes a socket whose send queue passes 16 MiB; replies wait
    // for it to drain below this instead.
    private const val MAX_QUEUED = 8L * 1024 * 1024

    private class FsError(val code: String, message: String) : Exception(message)

    private val STREAMS = setOf("fs.readstream", "fs.writestream")

    private val root: File by lazy { Environment.getExternalStorageDirectory().canonicalFile }

    fun hasAccess(ctx: Context): Boolean =
        if (Build.VERSION.SDK_INT >= 30) Environment.isExternalStorageManager()
        else ctx.checkSelfPermission(Manifest.permission.WRITE_EXTERNAL_STORAGE) == PackageManager.PERMISSION_GRANTED

    /**
     * Answers request [req] on [socket], off the caller's thread. Listed
     * directories are added to [watch]; [denied] runs when a request fails
     * because file access is off. Streams move their data over HTTPS to
     * [target], the computer's address and client, like file transfers.
     */
    fun serve(
        ctx: Context, socket: WebSocket, watch: FsWatch, denied: () -> Unit, target: Pair<String, OkHttpClient>?,
        req: String, type: String, body: JSONObject, payload: ByteArray?,
    ) = (if (type in STREAMS) streams else pool).execute {
        val reply = JSONObject().put("v", Proto.VERSION).put("type", "reply").put("ref", req)
        var data: ByteArray? = null
        if (type in CHANGES) for (k in listOf("path", "from", "to")) {
            body.optString(k).takeIf { it.isNotEmpty() }?.let { runCatching { markOwn(resolve(it).toString()) } }
        }
        try {
            if (!hasAccess(ctx)) {
                denied()
                throw FsError("perm", "file access isn't allowed on the phone")
            }
            val (b, d) = if (type in STREAMS) {
                stream(ctx, type, body, req, target ?: throw FsError("io", "not connected")) to null
            } else {
                handle(ctx, type, body, payload ?: ByteArray(0))
            }
            reply.put("body", b)
            data = d
            if (type == "fs.list") watch.listed(resolve(body.getString("path")).toFile(), body.getString("path"))
        } catch (e: Exception) {
            val (code, msg) = describe(e)
            if (code == "io") Log.w(TAG, "fs: $type failed", e)
            reply.put("error", JSONObject().put("code", code).put("message", msg))
        }
        // A dead socket never drains; the desktop has given up by then anyway.
        val giveUp = System.currentTimeMillis() + 30_000
        while (socket.queueSize() > MAX_QUEUED) {
            if (System.currentTimeMillis() > giveUp) return@execute
            Thread.sleep(20)
        }
        if (data != null) socket.send(Proto.binary(reply, data)) else socket.send(reply.toString())
    }

    private fun handle(ctx: Context, type: String, b: JSONObject, payload: ByteArray): Pair<JSONObject, ByteArray?> {
        when (type) {
            "fs.stat" -> return entry(resolve(b.getString("path"))) to null

            "fs.list" -> {
                val dir = resolve(b.getString("path"))
                val out = JSONArray()
                Files.newDirectoryStream(dir).use { s ->
                    for (p in s) {
                        try {
                            out.put(entry(p))
                        } catch (_: IOException) {
                            // vanished meanwhile
                        }
                    }
                }
                return JSONObject().put("entries", out) to null
            }

            "fs.read" -> {
                val p = resolve(b.getString("path"))
                val off = b.getLong("offset")
                val len = b.getInt("length")
                if (off < 0 || len < 0 || len > MAX_PAYLOAD) throw FsError("invalid", "bad offset or length")
                if (Files.isDirectory(p)) throw FsError("isdir", "is a directory")
                val buf = ByteBuffer.allocate(len)
                FileChannel.open(p, StandardOpenOption.READ).use { ch ->
                    while (buf.hasRemaining()) {
                        if (ch.read(buf, off + buf.position()) < 0) break
                    }
                }
                return JSONObject() to buf.array().copyOf(buf.position())
            }

            "fs.write" -> {
                val p = resolve(b.getString("path"))
                val off = b.getLong("offset")
                if (off < 0 || payload.size > MAX_PAYLOAD) throw FsError("invalid", "bad offset or length")
                if (Files.isDirectory(p)) throw FsError("isdir", "is a directory")
                // With "create", a whole small file in one request.
                val opts = if (b.optBoolean("create")) {
                    setOf(StandardOpenOption.WRITE, StandardOpenOption.CREATE, StandardOpenOption.TRUNCATE_EXISTING)
                } else {
                    setOf(StandardOpenOption.WRITE)
                }
                FileChannel.open(p, opts).use { ch ->
                    val buf = ByteBuffer.wrap(payload)
                    while (buf.hasRemaining()) ch.write(buf, off + buf.position())
                }
                changed(ctx, p)
                return entry(p) to null
            }

            "fs.create" -> {
                val p = resolve(b.getString("path"))
                if (Files.isDirectory(p)) throw FsError("isdir", "is a directory")
                val opts = mutableSetOf<OpenOption>(StandardOpenOption.WRITE)
                opts += if (b.optBoolean("exclusive")) StandardOpenOption.CREATE_NEW else StandardOpenOption.CREATE
                if (b.optBoolean("truncate")) opts += StandardOpenOption.TRUNCATE_EXISTING
                FileChannel.open(p, opts).close()
                changed(ctx, p)
                return entry(p) to null
            }

            "fs.mkdir" -> {
                val p = resolve(b.getString("path"))
                Files.createDirectory(p)
                return entry(p) to null
            }

            "fs.remove" -> {
                val p = resolve(b.getString("path"))
                if (isRoot(p)) throw FsError("perm", "can't remove the root")
                if (b.optBoolean("recursive") && Files.isDirectory(p, LinkOption.NOFOLLOW_LINKS)) {
                    deleteTree(p)
                } else {
                    Files.delete(p)
                }
                changed(ctx, p)
                return JSONObject() to null
            }

            "fs.rename" -> {
                val from = resolve(b.getString("from"))
                val to = resolve(b.getString("to"))
                if (isRoot(from) || isRoot(to)) throw FsError("perm", "can't move the root")
                if (b.optBoolean("no_replace")) Files.move(from, to)
                else Files.move(from, to, StandardCopyOption.REPLACE_EXISTING)
                changed(ctx, from)
                changed(ctx, to)
                return JSONObject() to null
            }

            "fs.setattr" -> {
                val p = resolve(b.getString("path"))
                if (b.has("size")) {
                    if (!Files.exists(p)) throw NoSuchFileException(p.toString())
                    if (Files.isDirectory(p)) throw FsError("isdir", "is a directory")
                    // Unlike FileChannel.truncate, this also extends.
                    RandomAccessFile(p.toFile(), "rw").use { it.setLength(b.getLong("size")) }
                    changed(ctx, p)
                }
                if (b.has("mtime")) Files.setLastModifiedTime(p, FileTime.fromMillis(b.getLong("mtime")))
                return entry(p) to null
            }

            "fs.statfs" -> {
                val st = StatFs(resolve(b.getString("path")).toString())
                return JSONObject().put("total", st.totalBytes).put("free", st.availableBytes) to null
            }
        }
        throw FsError("unsupported", type)
    }

    /**
     * fs.readstream: PUTs the file from the offset to its end. fs.writestream:
     * GETs the data and writes it into the file from the offset on.
     */
    private fun stream(ctx: Context, type: String, b: JSONObject, req: String, target: Pair<String, OkHttpClient>): JSONObject {
        val (base, client) = target
        val url = "$base/stream/$req"
        val p = resolve(b.getString("path"))
        val off = b.getLong("offset")
        if (off < 0) throw FsError("invalid", "bad offset")
        if (Files.isDirectory(p)) throw FsError("isdir", "is a directory")

        if (type == "fs.readstream") {
            val n = maxOf(Files.size(p) - off, 0)
            val data = object : RequestBody() {
                override fun contentType() = "application/octet-stream".toMediaType()
                override fun contentLength() = n
                override fun writeTo(sink: BufferedSink) {
                    FileChannel.open(p, StandardOpenOption.READ).use { ch ->
                        var pos = off
                        while (pos < off + n) {
                            val t = ch.transferTo(pos, off + n - pos, sink)
                            if (t <= 0) throw IOException("file shrank while sending it")
                            pos += t
                        }
                    }
                }
            }
            client.newCall(Request.Builder().url(url).put(data).build()).execute().use { r ->
                if (r.code != 204) throw IOException("PUT: ${r.code}")
            }
            return JSONObject()
        }

        if (!Files.exists(p)) throw NoSuchFileException(b.getString("path"))
        FileChannel.open(p, StandardOpenOption.WRITE).use { ch ->
            client.newCall(Request.Builder().url(url).build()).execute().use { r ->
                if (r.code != 200) throw IOException("GET: ${r.code}")
                val src = r.body.source()
                val buf = ByteBuffer.allocate(256 * 1024)
                var pos = off
                while (true) {
                    buf.clear()
                    if (src.read(buf) < 0) break
                    buf.flip()
                    while (buf.hasRemaining()) pos += ch.write(buf, pos)
                }
            }
        }
        changed(ctx, p)
        return entry(p)
    }

    /**
     * Maps a protocol path ("/DCIM/Camera") into shared storage, refusing
     * anything that resolves outside it, e.g. through ".." or a symlink.
     */
    private fun resolve(path: String): Path {
        val f = File(root, path.trimStart('/')).canonicalFile
        if (f != root && !f.path.startsWith(root.path + "/")) throw NoSuchFileException(path)
        return f.toPath()
    }

    private fun isRoot(p: Path) = p.toFile() == root

    private fun entry(p: Path): JSONObject {
        val a = Files.readAttributes(p, BasicFileAttributes::class.java)
        val name = if (isRoot(p)) "" else p.fileName.toString()
        return JSONObject()
            .put("name", name)
            .put("dir", a.isDirectory)
            .put("size", if (a.isDirectory) 0 else a.size())
            .put("mtime", a.lastModifiedTime().toMillis())
    }

    /** Deletes a directory tree without following symlinks out of it. */
    private fun deleteTree(dir: Path) {
        Files.walkFileTree(dir, object : SimpleFileVisitor<Path>() {
            override fun visitFile(file: Path, attrs: BasicFileAttributes): FileVisitResult {
                Files.delete(file)
                return FileVisitResult.CONTINUE
            }

            override fun postVisitDirectory(d: Path, exc: IOException?): FileVisitResult {
                if (exc != null) throw exc
                Files.delete(d)
                return FileVisitResult.CONTINUE
            }
        })
    }

    /**
     * Tells the media store about a changed path, so the gallery and other
     * apps see it. A file being written arrives in many chunks, so the scan
     * waits until the writes have stopped for a moment.
     */
    private fun changed(ctx: Context, p: Path) {
        val path = p.toString()
        markOwn(path)
        // One scan for a whole batch of files, once they've stopped coming.
        synchronized(toScan) {
            toScan += path
            scanJob?.cancel(false)
            scanJob = scanner.schedule({
                val paths = synchronized(toScan) { toScan.toTypedArray().also { toScan.clear() } }
                MediaScannerConnection.scanFile(ctx, paths, null, null)
            }, 2, TimeUnit.SECONDS)
        }
    }

    private fun markOwn(path: String) {
        val now = SystemClock.elapsedRealtime()
        own[path] = now
        if (own.size > 1000) own.entries.removeIf { now - it.value > OWN_MS }
    }

    /** Whether the computer itself changed [path] just now. */
    fun isOwn(path: String): Boolean {
        val t = own[path] ?: return false
        return SystemClock.elapsedRealtime() - t < OWN_MS
    }

    private fun describe(e: Exception): Pair<String, String> {
        val code = when (e) {
            is FsError -> e.code
            is NoSuchFileException -> "notfound"
            is FileAlreadyExistsException -> "exists"
            is DirectoryNotEmptyException -> "notempty"
            is NotDirectoryException -> "notdir"
            is AccessDeniedException, is SecurityException -> "perm"
            is JSONException, is IllegalArgumentException -> "invalid"
            // Other errno values only show in the reason text.
            is FileSystemException -> when {
                e.reason?.contains("Not a directory") == true -> "notdir"
                e.reason?.contains("Is a directory") == true -> "isdir"
                e.reason?.contains("No space") == true -> "nospace"
                e.reason?.contains("not empty") == true -> "notempty"
                else -> "io"
            }
            is IOException -> if (e.message?.contains("ENOSPC") == true) "nospace" else "io"
            else -> "io"
        }
        // Don't show the desktop where storage lives on the phone.
        val msg = (e.message ?: e.javaClass.simpleName).replace(root.path, "")
        return code to msg
    }
}

/**
 * Watches the directories a computer has recently listed and tells it when
 * one changes (fs.changed), so it can keep its cache of them. Watches end
 * when the connection drops, since the computer forgets its cache then.
 */
class FsWatch(private val send: (JSONObject) -> Unit) {
    private class Watched(val observer: FileObserver, var listed: Long)

    private val watched = LinkedHashMap<String, Watched>() // by protocol path
    private val pending = HashMap<String, ScheduledFuture<*>>()

    @Synchronized
    fun listed(dir: File, path: String) {
        val now = System.currentTimeMillis()
        watched.entries.removeAll { (_, w) ->
            (now - w.listed > EXPIRE_MS).also { if (it) w.observer.stopWatching() }
        }
        watched[path]?.let {
            it.listed = now
            return
        }
        while (watched.size >= MAX_WATCHED) {
            val oldest = watched.entries.minBy { it.value.listed }
            oldest.value.observer.stopWatching()
            watched.remove(oldest.key)
        }
        val observer = object : FileObserver(dir, MASK) {
            override fun onEvent(event: Int, name: String?) {
                if (!PhoneFs.isOwn(if (name == null) dir.path else File(dir, name).path)) changed(path)
            }
        }
        observer.startWatching()
        watched[path] = Watched(observer, now)
    }

    /** Coalesces a burst of events, e.g. a file being written, into one notification. */
    @Synchronized
    private fun changed(path: String) {
        if (path !in watched || pending.containsKey(path)) return
        pending[path] = timer.schedule({
            synchronized(this) { pending.remove(path) }
            send(JSONObject().put("path", path))
        }, DEBOUNCE_MS, TimeUnit.MILLISECONDS)
    }

    @Synchronized
    fun clear() {
        watched.values.forEach { it.observer.stopWatching() }
        watched.clear()
        pending.values.forEach { it.cancel(false) }
        pending.clear()
    }

    private companion object {
        const val MASK = FileObserver.CREATE or FileObserver.DELETE or FileObserver.MOVED_FROM or
            FileObserver.MOVED_TO or FileObserver.CLOSE_WRITE or FileObserver.ATTRIB or
            FileObserver.DELETE_SELF or FileObserver.MOVE_SELF
        const val MAX_WATCHED = 64
        const val EXPIRE_MS = 15 * 60 * 1000L
        const val DEBOUNCE_MS = 300L
        val timer = Executors.newSingleThreadScheduledExecutor { Thread(it, "pocketlink-fswatch") }
    }
}
