package dev.tether

import android.security.keystore.KeyGenParameterSpec
import android.security.keystore.KeyProperties
import java.math.BigInteger
import java.net.Socket
import java.security.KeyPairGenerator
import java.security.KeyStore
import java.security.MessageDigest
import java.security.Principal
import java.security.PrivateKey
import java.security.cert.CertificateException
import java.security.cert.X509Certificate
import java.security.spec.ECGenParameterSpec
import java.util.Date
import java.util.concurrent.TimeUnit
import javax.net.ssl.SSLContext
import javax.net.ssl.SSLEngine
import javax.net.ssl.X509ExtendedKeyManager
import javax.net.ssl.X509TrustManager
import javax.security.auth.x500.X500Principal
import okhttp3.OkHttpClient

/**
 * The phone's TLS identity: an EC P-256 key in the Android Keystore with the
 * self-signed certificate the Keystore generates for it. The desktop
 * identifies this phone by that certificate's SHA-256 fingerprint.
 */
object Identity {
    private const val ALIAS = "tether-identity"

    private val material: Pair<PrivateKey, X509Certificate> by lazy { loadOrCreate() }

    private fun loadOrCreate(): Pair<PrivateKey, X509Certificate> {
        val ks = KeyStore.getInstance("AndroidKeyStore").apply { load(null) }
        if (!ks.containsAlias(ALIAS)) {
            val now = System.currentTimeMillis()
            val day = TimeUnit.DAYS.toMillis(1)
            val gen = KeyPairGenerator.getInstance(KeyProperties.KEY_ALGORITHM_EC, "AndroidKeyStore")
            gen.initialize(
                KeyGenParameterSpec.Builder(ALIAS, KeyProperties.PURPOSE_SIGN)
                    .setAlgorithmParameterSpec(ECGenParameterSpec("secp256r1"))
                    // TLS hands the key a precomputed digest, hence DIGEST_NONE.
                    .setDigests(
                        KeyProperties.DIGEST_NONE,
                        KeyProperties.DIGEST_SHA256,
                        KeyProperties.DIGEST_SHA384,
                        KeyProperties.DIGEST_SHA512,
                    )
                    .setCertificateSubject(X500Principal("CN=tether"))
                    .setCertificateSerialNumber(BigInteger.valueOf(now))
                    .setCertificateNotBefore(Date(now - day))
                    .setCertificateNotAfter(Date(now + 30 * 365 * day))
                    .build()
            )
            gen.generateKeyPair()
        }
        val key = ks.getKey(ALIAS, null) as PrivateKey
        val cert = ks.getCertificate(ALIAS) as X509Certificate
        return key to cert
    }

    private val clients = HashMap<String, OkHttpClient>()

    /** An HTTP client that presents our identity and trusts only the desktop with [fp]. */
    @Synchronized
    fun client(fp: String): OkHttpClient = clients.getOrPut(fp) {
        val (key, cert) = material
        val tm = PinningTrustManager(fp)
        val ctx = SSLContext.getInstance("TLS")
        ctx.init(arrayOf(IdentityKeyManager(key, cert)), arrayOf(tm), null)
        OkHttpClient.Builder()
            .sslSocketFactory(ctx.socketFactory, tm)
            // The pinned fingerprint replaces hostname checks; we connect by IP.
            .hostnameVerifier { _, _ -> true }
            .connectTimeout(5, TimeUnit.SECONDS)
            .readTimeout(60, TimeUnit.SECONDS)
            .writeTimeout(60, TimeUnit.SECONDS)
            .pingInterval(2, TimeUnit.MINUTES)
            .build()
    }

    fun fingerprint(cert: X509Certificate): String =
        MessageDigest.getInstance("SHA-256").digest(cert.encoded).joinToString("") { "%02x".format(it) }

    private class IdentityKeyManager(
        private val key: PrivateKey,
        private val cert: X509Certificate,
    ) : X509ExtendedKeyManager() {
        override fun chooseClientAlias(keyType: Array<out String>?, issuers: Array<out Principal>?, socket: Socket?) = ALIAS
        override fun chooseEngineClientAlias(keyType: Array<out String>?, issuers: Array<out Principal>?, engine: SSLEngine?) = ALIAS
        override fun getClientAliases(keyType: String?, issuers: Array<out Principal>?) = arrayOf(ALIAS)
        override fun getCertificateChain(alias: String?) = arrayOf(cert)
        override fun getPrivateKey(alias: String?) = key
        override fun getServerAliases(keyType: String?, issuers: Array<out Principal>?): Array<String>? = null
        override fun chooseServerAlias(keyType: String?, issuers: Array<out Principal>?, socket: Socket?): String? = null
    }

    private class PinningTrustManager(private val fp: String) : X509TrustManager {
        override fun checkServerTrusted(chain: Array<out X509Certificate>, authType: String?) {
            val got = chain.firstOrNull()?.let(::fingerprint) ?: throw CertificateException("no certificate")
            if (!MessageDigest.isEqual(got.toByteArray(), fp.toByteArray())) {
                throw CertificateException("desktop certificate changed (got $got)")
            }
        }

        override fun checkClientTrusted(chain: Array<out X509Certificate>?, authType: String?) =
            throw CertificateException("not a server")

        override fun getAcceptedIssuers(): Array<X509Certificate> = emptyArray()
    }
}
