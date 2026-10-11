package io.github.carnager.pocketlink

import android.content.ContentValues
import android.content.Context
import android.net.Uri
import android.os.Environment
import android.os.SystemClock
import android.provider.DocumentsContract
import android.provider.MediaStore
import android.webkit.MimeTypeMap
import java.io.EOFException
import java.io.File
import java.io.IOException
import java.io.InputStream
import java.util.TreeMap
import java.util.concurrent.ConcurrentHashMap
import java.util.concurrent.ExecutorService
import java.util.concurrent.Executors
import java.util.concurrent.Future
import java.util.concurrent.Semaphore
import org.json.JSONArray
import org.json.JSONObject

/**
 * Unpacks a tar offer (several files or folders sent as one archive; see
 * FileOffer in the desktop's internal/proto) while it downloads.
 *
 * Files are created four at a time: on shared storage, creating a file
 * costs ~30 ms however it's done, but MediaStore handles several at once,
 * which makes many small files about six times faster than one by one.
 *
 * Progress lives in the offer's record [rec]: "done", the archive offset up
 * to which everything is unpacked for good, and "started", the files begun
 * after it. An interrupted transfer continues at "done", first deleting the
 * started files, so it neither loses nor duplicates any.
 */
internal class Unpacker(
    private val ctx: Context,
    private val rec: JSONObject,
    private val save: () -> Unit,
    private val onProgress: (bytes: Long, files: Int) -> Unit,
) {
    private val sink: Sink = Links.get(ctx).prefs.saveTree?.let { TreeSink(ctx, it, rec) } ?: DownloadsSink(ctx, rec)

    /** Unpacks the archive from [input], which starts at offset "done". */
    fun run(input: InputStream) {
        val start = rec.optLong("done", 0)
        val started = rec.optJSONArray("started") ?: JSONArray()
        for (i in 0 until started.length()) sink.delete(started.getString(i))
        rec.put("started", JSONArray())
        save()

        val pool = Executors.newFixedThreadPool(WORKERS) { Thread(it, "pocketlink-unpack") }
        try {
            unpack(TarIn(input, start), pool)
        } finally {
            pool.shutdownNow()
        }
    }

    private fun unpack(tar: TarIn, pool: ExecutorService) {
        val buffered = Semaphore(MAX_BUFFERED)
        // Entries since the last checkpoint, in archive order: end offset,
        // and the file's path while it's being written.
        val ends = TreeMap<Int, Long>()
        val writing = TreeMap<Int, String>()
        val finished = ConcurrentHashMap.newKeySet<Int>()
        val jobs = ArrayList<Future<*>>()
        var filesDone = rec.optInt("files_done", 0)
        var index = 0
        var lastSave = 0L

        fun checkpoint(force: Boolean) {
            jobs.removeAll { f -> f.isDone.also { if (it) f.get() } } // a failed write fails the transfer
            var advanced = false
            while (ends.isNotEmpty() && ends.firstKey() in finished) {
                val i = ends.firstKey()
                rec.put("done", ends.remove(i))
                if (writing.remove(i) != null) filesDone++
                finished.remove(i)
                advanced = true
            }
            val now = SystemClock.elapsedRealtime()
            if (advanced && (force || now - lastSave > 1000)) {
                lastSave = now
                rec.put("files_done", filesDone)
                rec.put("started", JSONArray(writing.values))
                save()
                onProgress(rec.getLong("done"), filesDone)
            }
        }

        while (true) {
            val e = tar.next() ?: break
            val i = index++
            val rel = safePath(e.name)
            when {
                rel == null || (e.type != TarIn.FILE && e.type != TarIn.DIR) -> {
                    tar.skip(e.size) // not something we unpack
                    ends[i] = tar.pos
                    finished += i
                }
                e.type == TarIn.DIR -> {
                    sink.mkdir(rel)
                    ends[i] = tar.pos
                    finished += i
                }
                else -> {
                    // Recorded before anything is created, so a crash can't
                    // leave a file behind that a retry doesn't know about.
                    writing[i] = rel
                    rec.put("started", JSONArray(writing.values))
                    save()
                    if (e.size <= SMALL) {
                        val data = tar.readData(e.size.toInt())
                        ends[i] = tar.pos
                        val kib = maxOf(1, data.size / 1024)
                        buffered.acquire(kib)
                        jobs += pool.submit {
                            try {
                                sink.write(rel, data)
                                finished += i
                            } finally {
                                buffered.release(kib)
                            }
                        }
                    } else {
                        sink.write(rel, tar.dataStream(e.size))
                        tar.skipPadding(e.size)
                        ends[i] = tar.pos
                        finished += i
                    }
                }
            }
            checkpoint(false)
        }
        for (f in jobs) f.get()
        checkpoint(true)
    }

    /** An archive path as safe relative path elements, or null to skip it. */
    private fun safePath(name: String): String? {
        val parts = name.trimEnd('/').split('/').filter { it.isNotEmpty() && it != "." }
        if (parts.isEmpty() || name.startsWith("/") || parts.any { it == ".." || it.contains('\u0000') }) return null
        return parts.joinToString("/")
    }

    private companion object {
        const val WORKERS = 4
        const val SMALL = 1L shl 20 // read into memory and written by a worker
        const val MAX_BUFFERED = 32 * 1024 // KiB of small files waiting to be written
    }
}

/** Where an archive's files go. Paths are relative to the archive's folder. */
private interface Sink {
    fun mkdir(rel: String)
    fun write(rel: String, data: ByteArray)
    fun write(rel: String, data: InputStream)
    fun delete(rel: String)
}

private fun mimeOf(name: String): String =
    MimeTypeMap.getSingleton().getMimeTypeFromExtension(name.substringAfterLast('.', "").lowercase())
        ?: "application/octet-stream" // keeps the name as it is; a mismatching type would get its extension appended

/**
 * Downloads/, or a new folder in it named after the archive, through
 * MediaStore. The folder is picked once and kept in the record.
 */
private class DownloadsSink(private val ctx: Context, rec: JSONObject) : Sink {
    private val resolver = ctx.contentResolver
    private val base: String = rec.optString("dest").ifEmpty {
        val name = rec.optString("name")
        val dl = Environment.getExternalStoragePublicDirectory(Environment.DIRECTORY_DOWNLOADS)
        val dest = if (name.isEmpty()) {
            Environment.DIRECTORY_DOWNLOADS
        } else {
            var cand = name
            var n = 1
            while (File(dl, cand).exists()) cand = "$name (${n++})"
            "${Environment.DIRECTORY_DOWNLOADS}/$cand"
        }
        rec.put("dest", dest)
        dest
    }

    private fun relDir(rel: String) = (base + "/" + rel.substringBeforeLast('/', "")).trimEnd('/') + "/"

    override fun mkdir(rel: String) {
        // MediaStore makes folders for the files in them; this is for empty ones.
        File(Environment.getExternalStorageDirectory(), "$base/$rel").mkdirs()
    }

    private fun create(rel: String): Uri {
        val name = rel.substringAfterLast('/')
        val values = ContentValues().apply {
            put(MediaStore.Downloads.DISPLAY_NAME, name)
            put(MediaStore.Downloads.RELATIVE_PATH, relDir(rel))
            put(MediaStore.Downloads.MIME_TYPE, mimeOf(name))
        }
        return resolver.insert(MediaStore.Downloads.EXTERNAL_CONTENT_URI, values) ?: throw IOException("could not create $rel")
    }

    override fun write(rel: String, data: ByteArray) {
        val uri = create(rel)
        resolver.openOutputStream(uri, "w")!!.use { it.write(data) }
    }

    override fun write(rel: String, data: InputStream) {
        val uri = create(rel)
        resolver.openOutputStream(uri, "w")!!.use { data.copyTo(it, 256 * 1024) }
    }

    override fun delete(rel: String) {
        val sel = "${MediaStore.Downloads.RELATIVE_PATH} = ? AND ${MediaStore.Downloads.DISPLAY_NAME} = ?"
        resolver.delete(MediaStore.Downloads.EXTERNAL_CONTENT_URI, sel, arrayOf(relDir(rel), rel.substringAfterLast('/')))
    }
}

/**
 * The folder chosen in settings, through the Storage Access Framework. An
 * archive with a name gets a new folder in it, kept in the record.
 */
private class TreeSink(ctx: Context, tree: Uri, rec: JSONObject) : Sink {
    private val resolver = ctx.contentResolver
    private val root: Uri = rec.optString("dest_doc").ifEmpty { null }?.let(Uri::parse) ?: run {
        var doc = DocumentsContract.buildDocumentUriUsingTree(tree, DocumentsContract.getTreeDocumentId(tree))
        val name = rec.optString("name")
        if (name.isNotEmpty()) {
            // The provider picks a free name if this one is taken.
            doc = DocumentsContract.createDocument(resolver, doc, DocumentsContract.Document.MIME_TYPE_DIR, name)
                ?: throw IOException("could not create folder $name")
        }
        rec.put("dest_doc", doc.toString())
        doc
    }
    private val dirs = ConcurrentHashMap<String, Uri>()

    /** The folder at [rel] ("" for the root), created if needed. */
    @Synchronized
    private fun dir(rel: String): Uri {
        if (rel.isEmpty()) return root
        dirs[rel]?.let { return it }
        val parent = dir(rel.substringBeforeLast('/', ""))
        val name = rel.substringAfterLast('/')
        val doc = child(parent, name)
            ?: DocumentsContract.createDocument(resolver, parent, DocumentsContract.Document.MIME_TYPE_DIR, name)
            ?: throw IOException("could not create folder $rel")
        dirs[rel] = doc
        return doc
    }

    private fun child(parent: Uri, name: String): Uri? {
        val children = DocumentsContract.buildChildDocumentsUriUsingTree(parent, DocumentsContract.getDocumentId(parent))
        resolver.query(children, arrayOf(DocumentsContract.Document.COLUMN_DOCUMENT_ID, DocumentsContract.Document.COLUMN_DISPLAY_NAME), null, null, null)?.use { c ->
            while (c.moveToNext()) {
                if (c.getString(1) == name) return DocumentsContract.buildDocumentUriUsingTree(parent, c.getString(0))
            }
        }
        return null
    }

    override fun mkdir(rel: String) {
        dir(rel)
    }

    private fun create(rel: String): Uri {
        val name = rel.substringAfterLast('/')
        return DocumentsContract.createDocument(resolver, dir(rel.substringBeforeLast('/', "")), mimeOf(name), name)
            ?: throw IOException("could not create $rel")
    }

    override fun write(rel: String, data: ByteArray) {
        resolver.openOutputStream(create(rel), "w")!!.use { it.write(data) }
    }

    override fun write(rel: String, data: InputStream) {
        resolver.openOutputStream(create(rel), "w")!!.use { data.copyTo(it, 256 * 1024) }
    }

    override fun delete(rel: String) {
        val doc = child(dir(rel.substringBeforeLast('/', "")), rel.substringAfterLast('/')) ?: return
        DocumentsContract.deleteDocument(resolver, doc)
    }
}

/**
 * Reads tar headers and data from a stream that starts at archive offset
 * [pos] (a header boundary). Handles ustar and the PAX and GNU long-name
 * extensions; Go's archive/tar on the desktop writes ustar, plus PAX for
 * long names.
 */
internal class TarIn(private val input: InputStream, var pos: Long) {
    class Entry(val name: String, val type: Char, val size: Long)

    private val block = ByteArray(512)

    /** The next entry, or null at the end of the archive. */
    fun next(): Entry? {
        var longName: String? = null
        var paxSize: Long? = null
        while (true) {
            if (!readBlock()) return null
            if (block.all { it == 0.toByte() }) return null // end-of-archive blocks
            val type = block[156].toInt().toChar()
            var size = parseSize(block, 124, 12)
            when (type) {
                'x' -> { // PAX header for the next entry
                    val pax = parsePax(String(readData(size.toInt()), Charsets.UTF_8))
                    pax["path"]?.let { longName = it }
                    pax["size"]?.toLongOrNull()?.let { paxSize = it }
                    continue
                }
                'g' -> {
                    skip(size)
                    continue
                }
                'L' -> {
                    longName = String(readData(size.toInt()), Charsets.UTF_8).trimEnd('\u0000')
                    continue
                }
            }
            paxSize?.let { size = it }
            val name = longName ?: run {
                val n = cstr(block, 0, 100)
                val prefix = if (String(block, 257, 5, Charsets.US_ASCII) == "ustar") cstr(block, 345, 155) else ""
                if (prefix.isEmpty()) n else "$prefix/$n"
            }
            val t = when (type) {
                '0', '\u0000', '7' -> FILE
                '5' -> DIR
                else -> type
            }
            return Entry(name, t, size)
        }
    }

    /** Reads an entry's data (and its padding) into memory. */
    fun readData(size: Int): ByteArray {
        val data = ByteArray(size)
        readFully(data, size)
        skipPadding(size.toLong())
        return data
    }

    /** The entry's data as a stream of exactly [size] bytes; padding is left. */
    fun dataStream(size: Long): InputStream = object : InputStream() {
        var left = size
        override fun read(): Int {
            val b = ByteArray(1)
            return if (read(b, 0, 1) < 0) -1 else b[0].toInt() and 0xff
        }

        override fun read(b: ByteArray, off: Int, len: Int): Int {
            if (left == 0L) return -1
            val n = input.read(b, off, minOf(len.toLong(), left).toInt())
            if (n < 0) throw EOFException("archive ends inside a file")
            left -= n
            pos += n
            return n
        }
    }

    fun skip(size: Long) {
        discard(size)
        skipPadding(size)
    }

    fun skipPadding(size: Long) = discard((512 - size % 512) % 512)

    private fun discard(n: Long) {
        var left = n
        val buf = ByteArray(8192)
        while (left > 0) {
            val k = input.read(buf, 0, minOf(left, buf.size.toLong()).toInt())
            if (k < 0) throw EOFException("archive ends early")
            left -= k
            pos += k
        }
    }

    private fun readBlock(): Boolean {
        var got = 0
        while (got < 512) {
            val n = input.read(block, got, 512 - got)
            if (n < 0) {
                if (got == 0) return false
                throw EOFException("archive ends inside a header")
            }
            got += n
        }
        pos += 512
        return true
    }

    private fun readFully(buf: ByteArray, size: Int) {
        var got = 0
        while (got < size) {
            val n = input.read(buf, got, size - got)
            if (n < 0) throw EOFException("archive ends inside a file")
            got += n
        }
        pos += size
    }

    companion object {
        const val FILE = 'f'
        const val DIR = 'd'

        private fun cstr(b: ByteArray, off: Int, len: Int): String {
            var end = off
            while (end < off + len && b[end] != 0.toByte()) end++
            return String(b, off, end - off, Charsets.UTF_8)
        }

        /** Octal, or base-256 for big sizes (high bit set). */
        private fun parseSize(b: ByteArray, off: Int, len: Int): Long {
            if (b[off].toInt() and 0x80 != 0) {
                var v = 0L
                for (i in off + 1 until off + len) v = (v shl 8) or (b[i].toLong() and 0xff)
                return v
            }
            val s = cstr(b, off, len).trim()
            return if (s.isEmpty()) 0 else s.toLong(8)
        }

        /** PAX records: "<length> <key>=<value>\n". */
        private fun parsePax(s: String): Map<String, String> {
            val out = HashMap<String, String>()
            val bytes = s.toByteArray(Charsets.UTF_8)
            var i = 0
            while (i < bytes.size) {
                val sp = (i until bytes.size).firstOrNull { bytes[it] == ' '.code.toByte() } ?: break
                val len = String(bytes, i, sp - i, Charsets.US_ASCII).toIntOrNull() ?: break
                if (len <= 0 || i + len > bytes.size) break
                val rec = String(bytes, sp + 1, i + len - sp - 2, Charsets.UTF_8) // without the newline
                val eq = rec.indexOf('=')
                if (eq > 0) out[rec.substring(0, eq)] = rec.substring(eq + 1)
                i += len
            }
            return out
        }
    }
}
