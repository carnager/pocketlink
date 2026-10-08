package dev.tether

import android.app.Notification
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.ContentValues
import android.content.Context
import android.content.Intent
import android.net.Uri
import android.os.SystemClock
import android.provider.DocumentsContract
import android.provider.MediaStore
import android.provider.OpenableColumns
import android.util.Log
import java.io.File
import java.io.FileOutputStream
import java.io.IOException
import java.io.InputStream
import java.io.RandomAccessFile
import java.net.URLEncoder
import java.util.UUID
import java.util.concurrent.Executors
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.RequestBody
import okio.BufferedSink
import org.json.JSONObject

/**
 * Resumable file transfer; see internal/xfer in the Go code. Pending
 * transfers are persisted and continue whenever the connection comes back.
 */
class Transfers(private val ctx: Context, private val link: Link) {
    private val exec = Executors.newSingleThreadExecutor { Thread(it, "tether-xfer") }
    private val uploads = TransferList(File(ctx.filesDir, "uploads.json"))
    private val downloads = TransferList(File(ctx.filesDir, "downloads.json"))
    private val upDir = File(ctx.cacheDir, "uploads")
    private val downDir = File(ctx.cacheDir, "downloads")
    private val nm = ctx.getSystemService(NotificationManager::class.java)
    private var lastProgress = 0L

    /**
     * Copies shared content into our cache first: the permission to read it
     * is temporary, and a local copy lets the upload resume at any time.
     */
    fun share(uris: List<Uri>) = guarded {
        upDir.mkdirs()
        for (uri in uris) {
            val name = displayName(uri)
            try {
                val id = UUID.randomUUID().toString()
                val file = File(upDir, id)
                ctx.contentResolver.openInputStream(uri)!!.use { input -> file.outputStream().use { input.copyTo(it) } }
                uploads.add(
                    JSONObject().put("id", id).put("name", name).put("size", file.length()).put("path", file.path)
                )
            } catch (e: Exception) {
                Log.w(TAG, "reading $uri", e)
                finished(name.hashCode(), "Could not read $name", e.message, null)
            }
        }
        runUploads()
    }

    /** The desktop offered a file (frame already acked). */
    fun offered(offer: JSONObject) = guarded {
        if (!downloads.contains(offer.getString("id"))) downloads.add(offer)
        runDownloads()
    }

    /** The connection came up: continue whatever is pending. */
    fun kick() = guarded {
        runUploads()
        runDownloads()
    }

    fun reset() = guarded {
        uploads.clear()
        downloads.clear()
        upDir.deleteRecursively()
        downDir.deleteRecursively()
    }

    /** Runs [task] on the transfer thread, logging what would otherwise vanish in the executor. */
    private fun guarded(task: () -> Unit) = exec.execute {
        try {
            task()
        } catch (e: Exception) {
            Log.e(TAG, "transfer task failed", e)
        }
    }

    private fun runUploads() {
        for (rec in uploads.all()) {
            val t = link.target ?: return
            try {
                upload(rec, t)
            } catch (e: IOException) {
                Log.i(TAG, "upload ${rec.getString("name")} interrupted: ${e.message}")
                return // resumes on the next connection
            }
        }
    }

    private fun upload(rec: JSONObject, target: Pair<String, OkHttpClient>) {
        val (base, client) = target
        val id = rec.getString("id")
        val name = rec.getString("name")
        val size = rec.getLong("size")
        val file = File(rec.getString("path"))
        val url = "$base/files/in/$id"

        repeat(3) {
            var offset = 0L
            client.newCall(Request.Builder().url(url).head().build()).execute().use { r ->
                when (r.code) {
                    404 -> {}
                    200 -> {
                        if (r.header(H_COMPLETE) != null) return uploadDone(rec)
                        offset = r.header(H_OFFSET)?.toLongOrNull() ?: 0
                    }
                    else -> throw IOException("HEAD: ${r.code}")
                }
            }
            val body = FileRangeBody(file, offset) { progress(id, "Sending $name", offset + it, size) }
            val req = Request.Builder().url(url).put(body)
                .header(H_NAME, URLEncoder.encode(name, "UTF-8"))
                .header(H_SIZE, size.toString())
                .header(H_OFFSET, offset.toString())
                .build()
            client.newCall(req).execute().use { r ->
                when (r.code) {
                    201 -> return uploadDone(rec)
                    204, 409 -> {} // not complete or offset moved: ask again
                    else -> throw IOException("PUT: ${r.code}")
                }
            }
        }
        throw IOException("upload did not complete")
    }

    private fun uploadDone(rec: JSONObject) {
        File(rec.getString("path")).delete()
        uploads.remove(rec.getString("id"))
        val name = rec.getString("name")
        finished(rec.getString("id").hashCode(), "Sent $name", "to ${link.prefs.serverName}", null)
    }

    private fun runDownloads() {
        for (offer in downloads.all()) {
            val t = link.target ?: return
            try {
                download(offer, t)
            } catch (e: IOException) {
                Log.i(TAG, "download ${offer.optString("name")} interrupted: ${e.message}")
                return
            }
        }
    }

    private fun download(offer: JSONObject, target: Pair<String, OkHttpClient>) {
        val (base, client) = target
        val id = offer.getString("id")
        val name = offer.getString("name")
        val size = offer.getLong("size")
        downDir.mkdirs()
        val part = File(downDir, id)
        val have = part.length()

        val req = Request.Builder().url("$base/files/out/$id")
            .apply { if (have > 0) header("Range", "bytes=$have-") }
            .build()
        client.newCall(req).execute().use { r ->
            when (r.code) {
                206 -> copy(r.body.byteStream(), part, append = true, id, name, have, size)
                200 -> copy(r.body.byteStream(), part, append = false, id, name, 0, size)
                416 -> if (have != size) {
                    part.delete()
                    throw IOException("range rejected")
                }
                404, 410 -> {
                    part.delete()
                    downloads.remove(id)
                    link.send("file.done", JSONObject().put("id", id))
                    finished(id.hashCode(), "Could not receive $name", "No longer available on the desktop", null)
                    return
                }
                else -> throw IOException("GET: ${r.code}")
            }
        }
        if (part.length() != size) throw IOException("incomplete")

        val uri = publish(part, name, offer.optString("mime", "application/octet-stream"))
        part.delete()
        downloads.remove(id)
        link.send("file.done", JSONObject().put("id", id))
        val view = Intent(Intent.ACTION_VIEW).setDataAndType(uri, offer.optString("mime"))
            .addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION or Intent.FLAG_ACTIVITY_NEW_TASK)
        finished(id.hashCode(), "Received $name", "from ${link.prefs.serverName}", view)
    }

    private fun copy(input: InputStream, part: File, append: Boolean, id: String, name: String, start: Long, size: Long) {
        FileOutputStream(part, append).use { out ->
            val buf = ByteArray(64 * 1024)
            var done = start
            while (true) {
                val n = input.read(buf)
                if (n < 0) break
                out.write(buf, 0, n)
                done += n
                progress(id, "Receiving $name", done, size)
            }
        }
    }

    /** Moves a finished download to the chosen folder, or Downloads. */
    private fun publish(part: File, name: String, mime: String): Uri {
        link.prefs.saveTree?.let { tree ->
            try {
                return publishToTree(tree, part, name, mime)
            } catch (e: Exception) {
                // Typically the folder was deleted or access was revoked.
                Log.w(TAG, "saving to chosen folder failed, using Downloads", e)
            }
        }
        return publishToDownloads(part, name, mime)
    }

    private fun publishToTree(tree: Uri, part: File, name: String, mime: String): Uri {
        val resolver = ctx.contentResolver
        val dir = DocumentsContract.buildDocumentUriUsingTree(tree, DocumentsContract.getTreeDocumentId(tree))
        // The provider picks a unique name if one is taken.
        val uri = DocumentsContract.createDocument(resolver, dir, mime, name)
            ?: throw IOException("could not create $name")
        try {
            resolver.openOutputStream(uri)!!.use { out -> part.inputStream().use { it.copyTo(out) } }
        } catch (e: Exception) {
            DocumentsContract.deleteDocument(resolver, uri)
            throw e
        }
        return uri
    }

    private fun publishToDownloads(part: File, name: String, mime: String): Uri {
        val resolver = ctx.contentResolver
        val values = ContentValues().apply {
            put(MediaStore.Downloads.DISPLAY_NAME, name)
            put(MediaStore.Downloads.MIME_TYPE, mime)
            put(MediaStore.Downloads.IS_PENDING, 1)
        }
        val uri = resolver.insert(MediaStore.Downloads.EXTERNAL_CONTENT_URI, values)
            ?: throw IOException("could not create download")
        try {
            resolver.openOutputStream(uri)!!.use { out -> part.inputStream().use { it.copyTo(out) } }
            resolver.update(uri, ContentValues().apply { put(MediaStore.Downloads.IS_PENDING, 0) }, null, null)
        } catch (e: Exception) {
            resolver.delete(uri, null, null)
            throw IOException("saving to Downloads: ${e.message}", e)
        }
        return uri
    }

    private fun displayName(uri: Uri): String {
        try {
            ctx.contentResolver.query(uri, arrayOf(OpenableColumns.DISPLAY_NAME), null, null, null)?.use { c ->
                if (c.moveToFirst()) c.getString(0)?.let { return it }
            }
        } catch (e: Exception) {
            Log.w(TAG, "querying name of $uri", e)
        }
        return uri.lastPathSegment ?: "file"
    }

    private fun progress(id: String, title: String, done: Long, total: Long) {
        val now = SystemClock.elapsedRealtime()
        if (now - lastProgress < 500 && done < total) return
        lastProgress = now
        val pct = if (total > 0) (done * 100 / total).toInt() else 100
        nm.notify(
            id.hashCode(),
            Notification.Builder(ctx, Notifs.TRANSFERS)
                .setSmallIcon(R.drawable.ic_stat)
                .setContentTitle(title)
                .setProgress(100, pct, false)
                .setOngoing(true)
                .setOnlyAlertOnce(true)
                .build()
        )
    }

    private fun finished(notifId: Int, title: String, text: String?, tap: Intent?) {
        val b = Notification.Builder(ctx, Notifs.FILES)
            .setSmallIcon(R.drawable.ic_stat)
            .setContentTitle(title)
            .setContentText(text)
            .setAutoCancel(true)
        if (tap != null) {
            b.setContentIntent(PendingIntent.getActivity(ctx, notifId, tap, PendingIntent.FLAG_IMMUTABLE))
        }
        nm.notify(notifId, b.build())
    }

    private class FileRangeBody(
        private val file: File,
        private val offset: Long,
        private val onProgress: (Long) -> Unit,
    ) : RequestBody() {
        override fun contentType() = "application/octet-stream".toMediaType()
        override fun contentLength() = file.length() - offset
        override fun writeTo(sink: BufferedSink) {
            RandomAccessFile(file, "r").use { raf ->
                raf.seek(offset)
                val buf = ByteArray(64 * 1024)
                var sent = 0L
                while (true) {
                    val n = raf.read(buf)
                    if (n < 0) break
                    sink.write(buf, 0, n)
                    sent += n
                    onProgress(sent)
                }
            }
        }
    }

    private companion object {
        const val TAG = "tether"
        const val H_NAME = "X-Tether-Name"
        const val H_SIZE = "X-Tether-Size"
        const val H_OFFSET = "X-Tether-Offset"
        const val H_COMPLETE = "X-Tether-Complete"
    }
}
