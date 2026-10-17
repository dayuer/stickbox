package com.yaodriver.stickbox.rasterkit

import com.yaodriver.stickbox.specs.NativeRasterKitSpec
import com.yaodriver.stickbox.specs.NativeWifiSetupSpec
import com.yaodriver.stickbox.wifisetup.WifiSetupModule
import com.facebook.react.BaseReactPackage
import com.facebook.react.bridge.NativeModule
import com.facebook.react.bridge.ReactApplicationContext
import com.facebook.react.module.model.ReactModuleInfo
import com.facebook.react.module.model.ReactModuleInfoProvider

/** app 自带的原生模块。它们在 app 里而不是独立的库，autolink 看不到。 */
class RasterKitPackage : BaseReactPackage() {
  override fun getModule(name: String, ctx: ReactApplicationContext): NativeModule? =
    when (name) {
      NativeRasterKitSpec.NAME -> RasterKitModule(ctx)
      NativeWifiSetupSpec.NAME -> WifiSetupModule(ctx)
      else -> null
    }

  override fun getReactModuleInfoProvider() = ReactModuleInfoProvider {
    listOf(NativeRasterKitSpec.NAME, NativeWifiSetupSpec.NAME).associateWith { name ->
      ReactModuleInfo(
        name, name,
        false,  // canOverrideExistingModule
        false,  // needsEagerInit
        false,  // isCxxModule
        true,   // isTurboModule
      )
    }
  }
}
