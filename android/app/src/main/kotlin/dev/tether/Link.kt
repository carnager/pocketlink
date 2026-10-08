package dev.tether

import android.content.Context
import android.net.ConnectivityManager
import android.net.Network
import android.net.Uri
import android.os.Build
import android.os.Handler
import android.os.Looper
import android.provider.Settings
import android.util.Log
import java.io.File
import java.util.concurrent.CopyOnWriteArrayList
import java.util.concurrent.Executors
import java.util.concurrent.ScheduledFuture
import java.util.concurrent.TimeUnit
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.Response
import okhttp3.WebSocket
import okhttp3.WebSocketListener
import org.json.JSONObject

enum class Status { UNPAIRED, CONNECTING, CONNECTED, OFFLINE, REJECTED }

fun Status.describe(desktop: String): String = when (this) {
    Status.UNPAIRED -> "Not paired"
    Status.CONNECTING -> "Connecting to $desktop…"
    Status.CONNECTED -> "Connected to $desktop"
    Status.OFFLINE -> "$desktop not reachable"
    Status.REJECTED -> "$desktop no longer trusts this phone. Pair again."
}

/**
 * The connection to the desktop. The phone always dials out; the connection
 * is expected to drop, and nothing depends on it staying up: frames wait in
 * the outbox until the desktop acks them.
 *
 * Reconnects are driven by network changes rather than polling, with an
 * exponential backoff as a fallback. All mutable state is confined to [exec].
 */
class Link private constructor(private val ctx: Context) {
    val prefs = Prefs(ctx)
    val transfers = Transfers(ctx, this)

    private val exec = Executors.newSingleThreadScheduledExecutor { Thread(it, "tether-link") }
    private val main = Handler(Looper.getMainLooper())
    private val outbox = Outbox(File(ctx.filesDir, "outbox.json"))

    private var started = false
    private var ws: WebSocket? = null
    private var generation = 0 // bumps whenever a socket is abandoned; stale callbacks are ignored
    private var connected = false
    private var connecting = false
    private var backoff = MIN_BACKOFF
    private var retry: ScheduledFuture<*>? = null
    private var network: Network? = null
    private val sent = HashSet<String>() // outbox IDs written on the current connection
    private val seen = object : LinkedHashMap<String, Unit>() {
        override fun removeEldestEntry(eldest: MutableMap.MutableEntry<String, Unit>?) = size > 512
    }

    /** Base URL and HTTP client for file transfers while connected. */
    @Volatile
    var target: Pair<String, OkHttpClient>? = null
        private set

    @Volatile
    var status: Status = if (prefs.server == null) Status.UNPAIRED else Status.OFFLINE
        private set

    private val observers = CopyOnWriteArrayList<(Status) -> Unit>()

    /** Calls [f] on the main thread with the current and future statuses. */
    fun observe(f: (Status) -> Unit): () -> Unit {
        observers.add(f)
        main.post { f(status) }
        return { observers.remove(f) }
    }

    fun start() = guarded {
        if (started) return@guarded
        started = true
        val cm = ctx.getSystemService(ConnectivityManager::class.java)
        network = cm.activeNetwork
        cm.registerDefaultNetworkCallback(object : ConnectivityManager.NetworkCallback() {
            override fun onAvailable(n: Network) = guarded { onNetwork(n) }
            override fun onLost(n: Network) = guarded {
                if (n == network) {
                    network = null
                    drop()
                }
            }
        })
        connect()
    }

    /** Queues a frame for the desktop. */
    fun send(type: String, body: JSONObject) = guarded {
        outbox.push(Proto.frame(type, body))
        flush()
    }

    /** Pairs using a QR link. [done] gets null on success or an error message. */
    fun pair(uri: String, done: (String?) -> Unit) = guarded {
        val offer = Offer.parse(uri)
        if (offer == null) {
            main.post { done("Not a tether pairing link") }
            return@guarded
        }
        drop()
        retry?.cancel(false)
        try {
            pairAt(offer, 0, mutableListOf(), done)
        } catch (e: Exception) {
            Log.e(TAG, "pairing failed", e)
            main.post { done(e.message ?: e.toString()) }
            connect()
        }
    }

    fun unpair() = guarded {
        drop()
        retry?.cancel(false)
        prefs.server = null
        outbox.clear()
        transfers.reset()
        setStatus(Status.UNPAIRED)
    }

    private fun onNetwork(n: Network) {
        if (n == network) return
        // The default network changed (e.g. Wi-Fi to mobile). A socket on the
        // old network is likely dead even if it doesn't know it yet.
        network = n
        drop()
        backoff = MIN_BACKOFF
        connect()
    }

    private fun connect() {
        retry?.cancel(false)
        retry = null
        if (connected || connecting) return
        val server = prefs.server
        if (server == null) {
            setStatus(Status.UNPAIRED)
            return
        }
        val last = prefs.lastAddr
        connecting = true
        if (status != Status.REJECTED) setStatus(Status.CONNECTING)
        tryAddr(server, server.addrs.sortedByDescending { it == last }, 0)
    }

    private fun tryAddr(server: Server, addrs: List<String>, i: Int) {
        if (i >= addrs.size) {
            connecting = false
            if (status != Status.REJECTED) setStatus(Status.OFFLINE)
            scheduleRetry()
            return
        }
        val addr = addrs[i]
        val gen = ++generation
        val client = Identity.client(server.fp)
        ws = client.newWebSocket(Request.Builder().url("wss://$addr/ws").build(), object : WebSocketListener() {
            override fun onOpen(webSocket: WebSocket, response: Response) = guarded {
                if (gen == generation) opened(webSocket, addr, client)
            }

            override fun onMessage(webSocket: WebSocket, text: String) = guarded {
                if (gen == generation) received(webSocket, text)
            }

            override fun onClosing(webSocket: WebSocket, code: Int, reason: String) {
                webSocket.close(1000, null)
            }

            override fun onClosed(webSocket: WebSocket, code: Int, reason: String) = guarded {
                if (gen == generation) lost(server, addrs, i, null, null)
            }

            override fun onFailure(webSocket: WebSocket, t: Throwable, response: Response?) = guarded {
                if (gen == generation) lost(server, addrs, i, t, response?.code)
            }
        })
    }

    private fun opened(socket: WebSocket, addr: String, client: OkHttpClient) {
        connecting = false
        connected = true
        backoff = MIN_BACKOFF
        prefs.lastAddr = addr
        target = "https://$addr" to client
        sent.clear()
        val hello = JSONObject().put("name", deviceName()).put("platform", "android")
        socket.send(Proto.frame("hello", hello, acked = false).toString())
        Log.i(TAG, "connected to $addr")
        setStatus(Status.CONNECTED)
        flush()
        transfers.kick()
    }

    private fun lost(server: Server, addrs: List<String>, i: Int, t: Throwable?, code: Int?) {
        ws = null
        if (connected) {
            Log.i(TAG, "disconnected: ${t?.message}")
            connected = false
            target = null
            setStatus(Status.OFFLINE)
            backoff = MIN_BACKOFF
            scheduleRetry()
            return
        }
        if (code == 403) {
            // We're not in the desktop's trust store (anymore).
            Log.w(TAG, "desktop rejected our certificate")
            connecting = false
            setStatus(Status.REJECTED)
            backoff = MAX_BACKOFF
            scheduleRetry()
            return
        }
        Log.d(TAG, "${addrs[i]} failed: ${t?.message}")
        tryAddr(server, addrs, i + 1)
    }

    private fun received(socket: WebSocket, text: String) {
        val f = try {
            JSONObject(text)
        } catch (e: Exception) {
            Log.w(TAG, "malformed frame", e)
            return
        }
        val id = f.optString("id")
        val body = f.optJSONObject("body") ?: JSONObject()
        when (val type = f.optString("type")) {
            "ack" -> {
                val ref = f.optString("ref")
                outbox.ack(ref)
                sent.remove(ref)
            }
            "hello" -> body.optString("name").takeIf { it.isNotEmpty() }?.let { prefs.serverName = it }
            else -> {
                // Retransmitted frames are acked again but handled only once.
                if (id.isEmpty() || seen.put(id, Unit) == null) handle(type, body)
                if (id.isNotEmpty()) socket.send(Proto.ack(id).toString())
            }
        }
    }

    private fun handle(type: String, body: JSONObject) {
        when (type) {
            "clip.set" -> Clip.set(ctx, body.optString("text"))
            "file.offer" -> transfers.offered(body)
            else -> Log.d(TAG, "ignoring $type")
        }
    }

    private fun flush() {
        val socket = ws ?: return
        if (!connected) return
        for (f in outbox.pending(System.currentTimeMillis())) {
            val id = f.getString("id")
            if (id !in sent && socket.send(f.toString())) sent.add(id)
        }
    }

    private fun pairAt(offer: Offer, i: Int, errors: MutableList<String>, done: (String?) -> Unit) {
        if (i >= offer.addrs.size) {
            main.post { done(errors.joinToString("\n")) }
            connect()
            return
        }
        val addr = offer.addrs[i]
        val url = "wss://$addr/ws?pair=${Uri.encode(offer.token)}&name=${Uri.encode(deviceName())}"
        val gen = ++generation
        Log.i(TAG, "pairing via $addr")
        exec.schedule({
            guarded {
                if (gen != generation) return@guarded
                generation++
                ws?.cancel()
                Log.w(TAG, "pairing via $addr timed out")
                errors.add("$addr: timed out")
                pairAt(offer, i + 1, errors, done)
            }
        }, PAIR_TIMEOUT_S, TimeUnit.SECONDS)
        ws = Identity.client(offer.fp).newWebSocket(Request.Builder().url(url).build(), object : WebSocketListener() {
            // The desktop sends hello only once it has accepted our certificate.
            override fun onMessage(webSocket: WebSocket, text: String) = guarded {
                if (gen != generation) return@guarded
                generation++
                webSocket.close(1000, null)
                ws = null
                prefs.server = Server(offer.name, offer.fp, offer.addrs)
                prefs.lastAddr = addr
                outbox.clear()
                transfers.reset()
                Log.i(TAG, "paired with ${offer.name} via $addr")
                main.post { done(null) }
                backoff = MIN_BACKOFF
                connect()
            }

            override fun onFailure(webSocket: WebSocket, t: Throwable, response: Response?) = guarded {
                if (gen != generation) return@guarded
                Log.w(TAG, "pairing via $addr failed (HTTP ${response?.code})", t)
                val why = if (response?.code == 403) "pairing code expired or already used" else (t.message ?: t.toString())
                errors.add("$addr: $why")
                pairAt(offer, i + 1, errors, done)
            }
        })
    }

    /** Abandons the current socket, if any. */
    private fun drop() {
        generation++
        ws?.cancel()
        ws = null
        connected = false
        connecting = false
        target = null
        if (prefs.server != null && status != Status.REJECTED) setStatus(Status.OFFLINE)
    }

    private fun scheduleRetry() {
        if (!started) return
        retry?.cancel(false)
        retry = exec.schedule({ connect() }, backoff, TimeUnit.MILLISECONDS)
        backoff = minOf(backoff * 2, MAX_BACKOFF)
    }

    /**
     * Runs [task] on the link thread. An exception would otherwise vanish
     * inside the executor and could leave the link without a connection or a
     * scheduled retry, so it's logged and the link reconnects.
     */
    private fun guarded(task: () -> Unit) = exec.execute {
        try {
            task()
        } catch (e: Exception) {
            Log.e(TAG, "link task failed", e)
            if (!connected && !connecting) scheduleRetry()
        }
    }

    private fun setStatus(s: Status) {
        if (status == s) return
        status = s
        main.post { observers.forEach { it(s) } }
    }

    private fun deviceName(): String =
        Settings.Global.getString(ctx.contentResolver, Settings.Global.DEVICE_NAME) ?: Build.MODEL

    companion object {
        private const val TAG = "tether"
        private const val MIN_BACKOFF = 1000L
        private const val MAX_BACKOFF = 5 * 60 * 1000L
        private const val PAIR_TIMEOUT_S = 15L

        @Volatile
        private var instance: Link? = null

        fun get(ctx: Context): Link = instance ?: synchronized(this) {
            instance ?: Link(ctx.applicationContext).also { instance = it }
        }
    }
}
